package session

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

// wait_subagents — blocking tool. Returns one row per requested id,
// waiting until each child terminates (or the call ctx fires).
// Reads parent's events first to short-circuit ids that already
// resolved (re-issued wait, restart-replay), then registers an
// activeToolFeed so the Run loop forwards live SubagentResult
// frames here.

const waitSubagentsSchema = `{
  "type": "object",
  "properties": {
    "ids": {
      "type": "array",
      "items": {"type": "string"},
      "minItems": 1
    }
  },
  "required": ["ids"]
}`

type waitSubagentsInput struct {
	IDs []string `json:"ids"`
}

type waitResultRow struct {
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
	Result    string `json:"result,omitempty"`
	Reason    string `json:"reason,omitempty"`
	TurnsUsed int    `json:"turns_used,omitempty"`
}

func (parent *Session) callWaitSubagents(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if parent.IsClosed() {
		return toolErr("session_gone", "calling session has already terminated")
	}
	var in waitSubagentsInput
	if err := json.Unmarshal(args, &in); err != nil {
		return toolErr("bad_request", fmt.Sprintf("invalid wait_subagents args: %v", err))
	}
	if len(in.IDs) == 0 {
		return toolErr("bad_request", "ids must be a non-empty array")
	}

	// First pass: collect any ids already terminal (cached in parent's
	// events as subagent_result OR the child's session_terminated)
	// before we register a feed. ListEvents lookup runs once per call;
	// the round-trip is cheap relative to a sub-agent's normal lifetime.
	collected := make(map[string]waitResultRow, len(in.IDs))
	if cached, err := drainCachedSubagentResults(ctx, parent, in.IDs); err == nil {
		maps.Copy(collected, cached)
	}

	pending := make(map[string]struct{}, len(in.IDs))
	for _, id := range in.IDs {
		if _, ok := collected[id]; ok {
			continue
		}
		pending[id] = struct{}{}
	}

	if len(pending) == 0 {
		return marshalWaitResults(in.IDs, collected)
	}

	// Register the active tool feed so the Run loop forwards matching
	// frames here. The loop reads activeToolFeed via atomic load
	// (session.go), so the store + clear is race-safe. BlockingState
	// declaratively flips the session into wait_subagents for the
	// duration of the block; the release closure flips it back to
	// active. Tool owns zero lifecycle code.
	//
	// Phase 5.1 § γ: the feed accepts not only SubagentResult but
	// also UserMessage (root reframe) and parent SystemMessage
	// (mission/worker reframe). On those interrupt frames the loop
	// short-circuits with a rendered tool-result instead of waiting
	// for completion; the missions in flight keep running.
	feedCh := make(chan protocol.Frame, len(pending)+1)
	feed := &ToolFeed{
		Consumes: func(f protocol.Frame) bool {
			switch f.Kind() {
			case protocol.KindSubagentResult:
				return true
			case protocol.KindUserMessage:
				return true
			case protocol.KindSystemMessage:
				return isParentNote(f, parent)
			}
			return false
		},
		Feed: func(f protocol.Frame) {
			select {
			case feedCh <- f:
			default:
				// Buffered to len(pending)+1; a full chan means a
				// duplicate frame for an id we already drained or
				// an interrupt arrived while another interrupt is
				// in flight — drop and let the next turn pick it
				// up via pendingInbound (UserMessage/SystemMessage
				// are dual-routed; see routeInbound).
			}
		},
		BlockingState:  protocol.SessionStatusWaitSubagents,
		BlockingReason: "tool=wait_subagents",
	}
	release := parent.registerToolFeed(ctx, feed)
	defer release()

	// Block until every pending id resolves, an interrupt frame
	// short-circuits, the parent's turn ctx cancels (/cancel), or
	// the call ctx fires.
	for len(pending) > 0 {
		select {
		case f := <-feedCh:
			switch v := f.(type) {
			case *protocol.SubagentResult:
				id := v.Payload.SessionID
				if id == "" {
					id = v.FromSessionID()
				}
				if _, want := pending[id]; !want {
					continue
				}
				row := waitResultRow{
					SessionID: id,
					Status:    statusFromReason(v.Payload.Reason),
					Result:    v.Payload.Result,
					Reason:    v.Payload.Reason,
					TurnsUsed: v.Payload.TurnsUsed,
				}
				collected[id] = row
				delete(pending, id)
				// Persist the consumed subagent_result into parent's
				// events so subsequent wait_subagents calls (or
				// restart) see the terminal state without rerunning
				// the child.
				if err := parent.emit(ctx, v); err != nil {
					parent.logger.Warn("session: wait_subagents: persist result",
						"parent", parent.id, "child", id, "err", err)
				}
			case *protocol.UserMessage:
				return marshalInterrupt(parent, pending, in.IDs, collected,
					"user_follow_up", v.Payload.Text, v)
			case *protocol.SystemMessage:
				return marshalInterrupt(parent, pending, in.IDs, collected,
					"parent_note", v.Payload.Content, v)
			}
		case <-ctx.Done():
			return toolErr("cancelled",
				fmt.Sprintf("wait_subagents aborted: %v", ctx.Err()))
		}
	}
	return marshalWaitResults(in.IDs, collected)
}

// isParentNote reports whether f is a SystemMessage authored by
// the session's direct parent — phase 5.1 § 3.4. Root sessions
// (parent == nil) can never receive a parent note.
func isParentNote(f protocol.Frame, s *Session) bool {
	if s.parent == nil {
		return false
	}
	if _, ok := f.(*protocol.SystemMessage); !ok {
		return false
	}
	return f.FromSessionID() == s.parent.id
}

// waitInterruptRow describes one in-flight child surfaced to the
// model alongside an interrupt — same shape for root's "active
// subagents" and mission's "active workers".
type waitInterruptRow struct {
	ID     string `json:"id"`
	Role   string `json:"role,omitempty"`
	Status string `json:"status"`
	Goal   string `json:"goal,omitempty"`
}

// waitInterruptResult is the tool-result envelope wait_subagents
// returns when an interrupt frame arrives. `instructions` carries
// the rendered reframe template; `pending` lists the ids that are
// still in flight; `resolved` keeps the same shape as the normal
// return so the caller can re-invoke wait_subagents and merge.
type waitInterruptResult struct {
	Interrupted  bool             `json:"interrupted"`
	Reason       string           `json:"reason"`
	Instructions string           `json:"instructions"`
	Pending      []waitInterruptRow `json:"pending,omitempty"`
	Resolved     []waitResultRow  `json:"resolved,omitempty"`
}

// marshalInterrupt renders the reframe template and packages it
// for the model. The active-child listing is built from the still-
// pending ids by reading the in-memory parent.children map for
// role/goal/status. Children that left the map (terminated between
// pending registration and the interrupt) surface as "unknown" so
// the listing length matches the pending set the model expects to
// resume waiting on.
func marshalInterrupt(parent *Session, pending map[string]struct{}, ids []string,
	collected map[string]waitResultRow, reason, content string,
	originator protocol.Frame) (json.RawMessage, error) {
	rows := pendingChildren(parent, pending)
	renderer := parent.deps.Prompts
	var instructions string
	switch reason {
	case "user_follow_up":
		instructions = strings.TrimRight(renderer.MustRender(
			"interrupts/follow_up_with_active_subagents",
			map[string]any{
				"UserMessage": content,
				"Subagents":   rows,
			},
		), "\n")
	case "parent_note":
		var parentRole, parentID string
		if parent.parent != nil {
			parentID = parent.parent.id
			parentRole = parent.parent.spawnRole
		}
		instructions = strings.TrimRight(renderer.MustRender(
			"interrupts/parent_note_with_active_workers",
			map[string]any{
				"ParentRole": parentRole,
				"ParentID":   parentID,
				"Content":    content,
				"Workers":    rows,
			},
		), "\n")
	}
	resolved := make([]waitResultRow, 0, len(collected))
	for _, id := range ids {
		if row, ok := collected[id]; ok {
			resolved = append(resolved, row)
		}
	}
	return json.Marshal(waitInterruptResult{
		Interrupted:  true,
		Reason:       reason,
		Instructions: instructions,
		Pending:      rows,
		Resolved:     resolved,
	})
}

// pendingChildren walks parent.children for ids in pending and
// returns one waitInterruptRow per pending id. Ids absent from the
// map (terminated mid-flight; transient race between Feed and
// children-map removal) surface as Status="unknown" so the model
// still sees the id in its action surface and can decide to drop
// it from a subsequent wait_subagents call.
func pendingChildren(parent *Session, pending map[string]struct{}) []waitInterruptRow {
	parent.childMu.Lock()
	defer parent.childMu.Unlock()
	rows := make([]waitInterruptRow, 0, len(pending))
	for id := range pending {
		child, ok := parent.children[id]
		if !ok || child == nil {
			rows = append(rows, waitInterruptRow{ID: id, Status: "unknown"})
			continue
		}
		rows = append(rows, waitInterruptRow{
			ID:     id,
			Role:   child.spawnRole,
			Status: child.Status(),
			Goal:   child.mission,
		})
	}
	return rows
}

func marshalWaitResults(ids []string, collected map[string]waitResultRow) (json.RawMessage, error) {
	out := make([]waitResultRow, 0, len(ids))
	for _, id := range ids {
		if row, ok := collected[id]; ok {
			out = append(out, row)
		}
	}
	return json.Marshal(out)
}

// drainCachedSubagentResults walks parent's events for already-
// observed subagent_result rows matching ids. Used to short-circuit
// wait_subagents when the parent re-asks for ids that already
// resolved (e.g. polling pattern, restart-replay).
func drainCachedSubagentResults(ctx context.Context, parent *Session, ids []string) (map[string]waitResultRow, error) {
	want := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		want[id] = struct{}{}
	}
	rows, err := parent.store.ListEvents(ctx, parent.id, store.ListEventsOpts{
		Kinds: []string{string(protocol.KindSubagentResult)},
		Limit: 1000,
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string]waitResultRow)
	for _, r := range rows {
		var p protocol.SubagentResultPayload
		if r.Metadata != nil {
			if b, err := json.Marshal(r.Metadata); err == nil {
				_ = json.Unmarshal(b, &p)
			}
		}
		id := p.SessionID
		if id == "" {
			if v, ok := r.Metadata["__from_session"].(string); ok {
				id = v
			}
		}
		if _, match := want[id]; !match {
			continue
		}
		out[id] = waitResultRow{
			SessionID: id,
			Status:    statusFromReason(p.Reason),
			Result:    p.Result,
			Reason:    p.Reason,
			TurnsUsed: p.TurnsUsed,
		}
	}
	return out, nil
}

// statusFromReason maps a session_terminated.reason to the
// wait_subagents status enum exposed to the LLM. The status is the
// stable machine-readable handle; reason carries free-form context.
func statusFromReason(reason string) string {
	switch {
	case reason == protocol.TerminationCompleted:
		return "completed"
	case reason == protocol.TerminationHardCeiling:
		return "hard_ceiling"
	case reason == protocol.TerminationCancelCascade:
		return "cancel_cascade"
	case reason == protocol.TerminationRestartDied:
		return "restart_died"
	case strings.HasPrefix(reason, protocol.TerminationSubagentCancelPrefix):
		return "subagent_cancel"
	case strings.HasPrefix(reason, protocol.TerminationPanicPrefix):
		return "panic"
	default:
		return "completed"
	}
}
