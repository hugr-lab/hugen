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
	// SubagentResult frames here. The loop reads activeToolFeed via
	// atomic load (session.go), so the store + clear is race-safe.
	// BlockingState declaratively flips the session into
	// wait_subagents for the duration of the block; the release
	// closure flips it back to active. Tool owns zero lifecycle code.
	feedCh := make(chan *protocol.SubagentResult, len(pending))
	feed := &ToolFeed{
		Consumes: func(f protocol.Frame) bool {
			return f.Kind() == protocol.KindSubagentResult
		},
		Feed: func(f protocol.Frame) {
			sr, ok := f.(*protocol.SubagentResult)
			if !ok {
				return
			}
			select {
			case feedCh <- sr:
			default:
				// Buffered channel sized to len(pending); a full chan
				// means a duplicate result for an id we already drained
				// — drop it (subagent_result is exactly-once per child).
			}
		},
		BlockingState:  protocol.SessionStatusWaitSubagents,
		BlockingReason: "tool=wait_subagents",
	}
	release := parent.registerToolFeed(ctx, feed)
	defer release()

	// Block until every pending id resolves, the parent's turn ctx
	// cancels (/cancel), or the call ctx fires.
	for len(pending) > 0 {
		select {
		case sr := <-feedCh:
			id := sr.Payload.SessionID
			if id == "" {
				id = sr.FromSessionID()
			}
			if _, want := pending[id]; !want {
				continue
			}
			row := waitResultRow{
				SessionID: id,
				Status:    statusFromReason(sr.Payload.Reason),
				Result:    sr.Payload.Result,
				Reason:    sr.Payload.Reason,
				TurnsUsed: sr.Payload.TurnsUsed,
			}
			collected[id] = row
			delete(pending, id)
			// Persist the consumed subagent_result into parent's events
			// so subsequent wait_subagents calls (or restart) see the
			// terminal state without rerunning the child.
			if err := parent.emit(ctx, sr); err != nil {
				parent.logger.Warn("session: wait_subagents: persist result",
					"parent", parent.id, "child", id, "err", err)
			}
		case <-ctx.Done():
			return toolErr("cancelled",
				fmt.Sprintf("wait_subagents aborted: %v", ctx.Err()))
		}
	}
	return marshalWaitResults(in.IDs, collected)
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
