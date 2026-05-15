package session

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"sort"
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
      "description": "Optional. Empty array (or absent) waits for ALL direct sub-agents of the calling session — the common 'sync after a wave' pattern. Provide explicit ids only when selectively waiting on a subset; unknown ids return an error listing the session's actual direct children."
    }
  }
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

	// Phase 5.1b: `ids` is optional. Empty / absent means "wait for
	// every direct sub-agent currently in flight" — the common case
	// after a fire-and-forget spawn cohort. Snapshot the live
	// children once and use the set for both the implicit
	// "wait for all" path and explicit-ids validation (the latter
	// guards against the LLM hallucinating ids like
	// "mission_id_from_step1" instead of substituting the real id
	// from a prior tool result — observed on Gemma 26B).
	parent.childMu.Lock()
	liveChildren := make(map[string]struct{}, len(parent.children))
	for id, c := range parent.children {
		if c != nil {
			liveChildren[id] = struct{}{}
		}
	}
	parent.childMu.Unlock()

	if len(in.IDs) == 0 {
		if len(liveChildren) == 0 {
			// Nothing to wait for — return empty results immediately
			// rather than blocking forever. The model can re-issue
			// after spawning if it intended a different cohort.
			return marshalWaitResults(nil, map[string]waitResultRow{})
		}
		in.IDs = make([]string, 0, len(liveChildren))
		for id := range liveChildren {
			in.IDs = append(in.IDs, id)
		}
		// Stable order keeps the model's view deterministic across
		// runs and makes test assertions predictable.
		sort.Strings(in.IDs)
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

	// Validate every pending id — terminated children already in
	// `collected` get a free pass since their results exist in
	// parent's events. Anything else MUST be a current direct
	// child; otherwise the call would block forever waiting for a
	// SubagentResult that will never arrive. Fast-fail with the
	// real child list so the model can correct its argument.
	for id := range pending {
		if _, live := liveChildren[id]; live {
			continue
		}
		realChildren := make([]string, 0, len(liveChildren))
		for c := range liveChildren {
			realChildren = append(realChildren, c)
		}
		sort.Strings(realChildren)
		return toolErr("not_a_child",
			fmt.Sprintf("subagent %q is not a direct child of this session; current direct children: %v", id, realChildren))
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
	case strings.HasPrefix(reason, protocol.TerminationUserCancelPrefix):
		return "user_cancel"
	case strings.HasPrefix(reason, protocol.TerminationPanicPrefix):
		return "panic"
	default:
		return "completed"
	}
}
