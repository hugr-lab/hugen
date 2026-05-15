package session

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// subagent_dismiss — accept a parked child's result and tear it
// down cleanly. Counterpart to session:subagent_cancel: cancel
// stops a still-running child, dismiss closes a child whose
// terminal result has already landed in the parent's history and
// is now sitting in awaiting_dismissal.
//
// Strict on the lifecycle state: dismiss called on a still-active
// child returns `not_parked` so the model uses subagent_cancel
// instead. Idempotent on a child that already finished tearing
// down (returns ok=true with already_gone=true) so a duplicate
// dismiss from a race or restart doesn't surface as an error.
//
// Phase 5.2 subagent-lifetime γ.

const subagentDismissSchema = `{
  "type": "object",
  "properties": {
    "session_id": {"type": "string", "description": "Direct child session id. Must be in awaiting_dismissal state — call subagent_cancel for still-running children."}
  },
  "required": ["session_id"]
}`

type subagentDismissInput struct {
	SessionID string `json:"session_id"`
}

type subagentDismissOutput struct {
	OK         bool `json:"ok"`
	AlreadyGone bool `json:"already_gone,omitempty"`
}

const dismissCloseReason = "subagent_dismissed"

func (parent *Session) callSubagentDismiss(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if parent.IsClosed() {
		return toolErr("session_gone", "calling session has already terminated")
	}
	var in subagentDismissInput
	if err := json.Unmarshal(args, &in); err != nil {
		return toolErr("bad_request", fmt.Sprintf("invalid subagent_dismiss args: %v", err))
	}
	if in.SessionID == "" {
		return toolErr("bad_request", "session_id is required")
	}

	parent.childMu.Lock()
	child, live := parent.children[in.SessionID]
	parent.childMu.Unlock()

	if !live || child == nil {
		// Child not in the live map — either never a child of caller
		// or it already finished teardown. Confirm direct parentage in
		// the store; surface not_a_child for genuine misuse, otherwise
		// the dismiss is idempotent.
		if errFrame := parent.assertChildOf(ctx, in.SessionID); errFrame != nil {
			return errFrame, nil
		}
		return json.Marshal(subagentDismissOutput{OK: true, AlreadyGone: true})
	}
	if child.IsClosed() {
		return json.Marshal(subagentDismissOutput{OK: true, AlreadyGone: true})
	}
	if !childIsParked(child) {
		return toolErr("not_parked",
			fmt.Sprintf("session %q is not parked (state=%q). Use subagent_cancel to terminate an active child.",
				in.SessionID, child.Status()))
	}

	closeFrame := protocol.NewSessionClose(child.id, parent.agent.Participant(), dismissCloseReason)
	child.Submit(ctx, closeFrame)
	select {
	case <-child.Done():
	case <-ctx.Done():
		return toolErr("cancelled", ctx.Err().Error())
	}
	// handleSubagentResult does the children map cleanup when the
	// projected SubagentResult lands on parent's inbox. Same
	// invariant as subagent_cancel.
	return json.Marshal(subagentDismissOutput{OK: true})
}
