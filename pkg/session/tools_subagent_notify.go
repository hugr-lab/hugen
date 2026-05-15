package session

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// notify_subagent — root / mission directs a focused note to a
// direct child. Behaviour depends on the child's lifecycle state:
//
//   - Active / wait_* / idle child: the note rides KindSystemMessage
//     with FromSession == caller.id; the child's wait_subagents
//     predicate (isParentNote) or buffered drain renders it.
//     Phase 5.1 § 3.4 / § 3.5.
//   - awaiting_dismissal (parked) child: the note becomes a synthetic
//     UserMessage authored by the parent agent. The child's Run loop
//     reads it as the next user turn — startTurn allocates fresh
//     turnState (budget reset) and the child re-enters its model
//     loop. Phase 5.2 subagent-lifetime γ.
//
// Same tool, contract switches on state. The caller does not need
// to know which mode applies — "deliver this directive" is the
// instruction.

const notifySubagentSchema = `{
  "type": "object",
  "properties": {
    "subagent_id": {"type": "string", "description": "Direct child session id. Must already be in the caller's children map."},
    "content":     {"type": "string", "description": "Focused directive crafted by the caller. NOT raw user text — the caller is responsible for translating the user's intent into a child-actionable note. For parked children (awaiting_dismissal), this is delivered as a UserMessage that triggers a new turn loop."},
    "urgent":      {"type": "boolean", "description": "Prepends '(urgent) ' to the content. No separate flag is carried on the frame. Honoured for both active and parked children."}
  },
  "required": ["subagent_id", "content"]
}`

type notifySubagentInput struct {
	SubagentID string `json:"subagent_id"`
	Content    string `json:"content"`
	Urgent     bool   `json:"urgent,omitempty"`
}

type notifySubagentOutput struct {
	Delivered bool   `json:"delivered"`
	FrameID   string `json:"frame_id"`
	// Rearmed is set when the call re-armed a parked child (parked →
	// active via synthetic UserMessage). Callers can render this as
	// a UX hint ("mission resumed"); routing logic does not branch
	// on it. Phase 5.2 γ.
	Rearmed bool `json:"rearmed,omitempty"`
}

func (parent *Session) callNotifySubagent(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if parent.IsClosed() {
		return toolErr("session_gone", "calling session has already terminated")
	}
	var in notifySubagentInput
	if err := json.Unmarshal(args, &in); err != nil {
		return toolErr("bad_request", fmt.Sprintf("invalid notify_subagent args: %v", err))
	}
	if in.SubagentID == "" {
		return toolErr("bad_request", "subagent_id is required")
	}
	if in.Content == "" {
		return toolErr("bad_request", "content is required")
	}

	// Direct-child-only validation. The dispatcher is the sole
	// authority on the caller→child relationship; the
	// permission stack handles role-tier gating
	// (`hugen:subagent:notify` is granted to _root / _mission, not
	// _worker).
	parent.childMu.Lock()
	child, ok := parent.children[in.SubagentID]
	parent.childMu.Unlock()
	if !ok || child == nil {
		return toolErr("not_a_child",
			fmt.Sprintf("session %q is not a direct child of the caller", in.SubagentID))
	}
	if child.IsClosed() {
		return toolErr("session_gone",
			fmt.Sprintf("subagent %q has already terminated", in.SubagentID))
	}

	content := in.Content
	if in.Urgent {
		content = "(urgent) " + content
	}

	// Phase 5.2 γ — parked-child re-arm path. UserMessage authored
	// by the parent's agent participant. Child's routeInbound sees
	// no active turn and calls startTurn, which allocates fresh
	// turnState (per-invocation budget reset). The synthetic-
	// UserMessage-as-agent pattern mirrors kickAsyncSummaryTurn
	// from phase 5.1c.async-root; replay paths already filter
	// these out of user-visible spans (see TUI replay.go).
	//
	// Phase 5.2 ε: clear the idle-timeout timer before Submit so a
	// stale fire can't auto-dismiss the child mid-re-arm. The hard
	// ceiling (lifetimeToolTurns vs st.capHard) stays armed — only
	// the soft cap resets per invocation.
	if childIsParked(child) {
		cancelParkIdleTimer(child)
		userMsg := protocol.NewUserMessage(child.ID(), parent.agent.Participant(), content)
		settled := child.Submit(ctx, userMsg)
		select {
		case <-settled:
		case <-ctx.Done():
			return toolErr("cancelled",
				fmt.Sprintf("notify_subagent aborted: %v", ctx.Err()))
		}
		if child.IsClosed() {
			return toolErr("session_gone",
				fmt.Sprintf("subagent %q terminated before the re-arm landed", in.SubagentID))
		}
		return json.Marshal(notifySubagentOutput{
			Delivered: true,
			FrameID:   userMsg.BaseFrame.ID,
			Rearmed:   true,
		})
	}

	frame := protocol.NewSystemMessage(child.ID(), parent.agent.Participant(),
		protocol.SystemMessageParentNote, content)
	// Author is parent's participant; FromSession carries parent's
	// id so child.isParentNote(f, child) returns true. Without
	// FromSession the wait_subagents feed cannot distinguish a
	// parent note from a self-emitted system message.
	frame.BaseFrame.FromSession = parent.id

	settled := child.Submit(ctx, frame)
	select {
	case <-settled:
	case <-ctx.Done():
		return toolErr("cancelled",
			fmt.Sprintf("notify_subagent aborted: %v", ctx.Err()))
	}
	if child.IsClosed() {
		// Race: child terminated between the IsClosed check above and
		// the Submit settling. Surface the same shape as a not-found
		// child so the caller's model handles both consistently.
		return toolErr("session_gone",
			fmt.Sprintf("subagent %q terminated before the note landed", in.SubagentID))
	}
	return json.Marshal(notifySubagentOutput{
		Delivered: true,
		FrameID:   frame.BaseFrame.ID,
	})
}
