package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// subagent_cancel — terminates one of the calling session's direct
// children with a stated reason. Cascades to descendants via the
// child's ctx (parent.ctx → child.ctx is a derived ctx). Phase
// 4.1b-pre stage B routed cancellation through the SessionClose
// Frame so the child's Run loop drives its own teardown — the
// caller blocks on child.Done() to keep the contract synchronous
// from the LLM's point of view.

const subagentCancelSchema = `{
  "type": "object",
  "properties": {
    "session_id": {"type": "string", "description": "Target identifier — either the subagent's short Name (returned by spawn) or its session_id. Must resolve to a direct child of the caller."},
    "reason":     {"type": "string"}
  },
  "required": ["session_id"]
}`

type subagentCancelInput struct {
	SessionID string `json:"session_id"`
	Reason    string `json:"reason,omitempty"`
}

type subagentCancelOutput struct {
	OK bool `json:"ok"`
}

func (parent *Session) callSubagentCancel(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if parent.IsClosed() {
		return toolErr("session_gone", "calling session has already terminated")
	}
	var in subagentCancelInput
	if err := json.Unmarshal(args, &in); err != nil {
		return toolErr("bad_request", fmt.Sprintf("invalid subagent_cancel args: %v", err))
	}
	if in.SessionID == "" {
		return toolErr("bad_request", "session_id is required")
	}

	// Direct child lookup — Manager is root-only post-pivot 4 and a
	// session only ever cancels its OWN immediate children. Walking
	// the descendant tree is forbidden: each session is the source of
	// truth for its direct sub-tree and any deeper cancel must travel
	// through that owner.
	//
	// Phase 5.2 α.1b: target accepts session_id OR Name. When the
	// caller passes a Name, resolveChildTarget rewrites it to the
	// canonical session_id for the rest of the flow (cancel frame,
	// assertChildOf store fallback). Closed children that already
	// left s.children fall through to the store-level assertion
	// using the input as a session_id.
	resolvedID, child, live := parent.resolveChildTarget(in.SessionID)
	if !live {
		// Name lookup miss → treat input as session_id for the
		// post-mortem assertion path below.
		resolvedID = in.SessionID
	}

	if live {
		if child.IsClosed() {
			return json.Marshal(subagentCancelOutput{OK: true})
		}
		reason := protocol.TerminationSubagentCancelPrefix + strings.TrimSpace(in.Reason)
		// Cancel travels through the SessionClose Frame so the
		// child's Run loop drives its own teardown (writes
		// session_terminated with the prefixed reason and pushes it
		// onto the outbox where the parent's pump projects a
		// SubagentResult — phase 4.1c).
		closeFrame := protocol.NewSessionClose(child.id, parent.agent.Participant(), reason)
		child.Submit(ctx, closeFrame)
		select {
		case <-child.Done():
		case <-ctx.Done():
			return toolErr("cancelled", ctx.Err().Error())
		}
		// parent.children cleanup: handleSubagentResult fires when
		// the projected SubagentResult arrives on parent's inbox. We
		// let that callback do the deregister so the invariant stays
		// single-sourced.
		return json.Marshal(subagentCancelOutput{OK: true})
	}

	// Not in the live children map — either already-terminal (the
	// goroutine exited and the deregister callback removed it) or
	// not a child of caller at all. Confirm direct parentage in the
	// store; not_a_child / session_not_found surface the wiring error,
	// otherwise the cancel is idempotent ok=true.
	if errFrame := parent.assertChildOf(ctx, resolvedID); errFrame != nil {
		return errFrame, nil
	}
	return json.Marshal(subagentCancelOutput{OK: true})
}
