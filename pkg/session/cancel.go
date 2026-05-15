package session

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// ErrCancelEmptyID is returned by RequestChildCancel when the caller
// passes an empty session id.
var ErrCancelEmptyID = errors.New("session: cancel: child id is required")

// ErrChildNotParked is returned by RequestChildDismiss when the
// target child is alive but not in awaiting_dismissal — operator
// should use RequestChildCancel for still-running children.
var ErrChildNotParked = errors.New("session: dismiss: child is not parked")

// RequestChildCancel asks the named direct child to terminate
// fast. Fire-and-forget — unlike the model-facing
// `session:subagent_cancel` tool this does NOT block on
// child.Done(); the parent's pump projects the resulting
// SubagentResult onto the parent's outbox when the child exits.
//
// Fast-cancel discipline (phase 5.1c.cancel-ux):
//
//  1. Submit a Cancel{Cascade:true} Frame. handleCancel calls
//     turnCancel() on the child, which propagates ctx.Done into
//     every in-flight tool dispatch and the model stream; the
//     cascade flag fires SessionClose{cancel_cascade} into every
//     direct grandchild, recursively. Workers under a cancelled
//     mission unwind immediately instead of waiting for the
//     mission's current turn to drain.
//  2. Submit a SessionClose{user_cancel:<reason>} Frame. This
//     terminates the child itself (Cancel alone only aborts the
//     turn). The user_cancel: prefix is in closeTurnSkipReason's
//     skip list so the dying session does NOT run a slow
//     findings-recording close turn — operator intent is "stop
//     now", not "wrap up".
//
// Idempotent: a child that is already closed (or not in the live
// children map) returns nil. Returns ErrCancelEmptyID when
// childID is empty.
// Return values:
//   - (false, ErrCancelEmptyID) — empty childID.
//   - (false, nil) — id is not in the live children map (typo OR
//     already-terminated child that was already swept). The slash
//     handler surfaces this as a usage_error so an operator typo
//     does not get silently confirmed.
//   - (true, nil) — Cancel + SessionClose were dispatched; the
//     async teardown is observed via the standard SubagentResult
//     projection.
//   - (false, ctx.Err()) — caller's ctx fired while awaiting the
//     Cancel Frame settle on the child's inbound channel.
func (s *Session) RequestChildCancel(ctx context.Context, childID, reason string) (bool, error) {
	if strings.TrimSpace(childID) == "" {
		return false, ErrCancelEmptyID
	}
	s.childMu.Lock()
	child, live := s.children[childID]
	s.childMu.Unlock()
	if !live || child.IsClosed() {
		return false, nil
	}
	full := protocol.TerminationUserCancelPrefix + strings.TrimSpace(reason)
	// Step 1: Cancel with cascade — aborts the child's in-flight
	// turn and cascade-closes its workers. Submit is async (spawns a
	// goroutine racing on s.in); we MUST await its `settled` channel
	// before submitting the SessionClose so the Run loop reads
	// Cancel first. Without the wait, the two goroutines race and
	// SessionClose can land in s.in ahead of Cancel — routeInbound's
	// SessionClose case returns sessionCloseSignal and the Cancel is
	// dropped silently when s.in closes during teardown. Reproduced
	// deterministically in PR-review test loops.
	cancelFrame := protocol.NewCancel(child.id, s.agent.Participant(), full)
	cancelFrame.Payload.Cascade = true
	cancelSettled := child.Submit(ctx, cancelFrame)
	select {
	case <-cancelSettled:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	// Step 2: SessionClose — terminates the child itself with a
	// skip-close-turn reason so teardown does not block on a
	// findings model turn the operator did not ask for. Fire-and-
	// forget; ordering vs Cancel is now guaranteed by the await
	// above.
	closeFrame := protocol.NewSessionClose(child.id, s.agent.Participant(), full)
	child.Submit(ctx, closeFrame)
	return true, nil
}

// RequestAllChildrenCancel snapshots the live children map and
// dispatches RequestChildCancel against each. Returns the ids it
// submitted cancels for (a child terminating between the snapshot
// and Submit still counts — Submit on a closed child is a no-op).
//
// Used by `/cancel_all_subagents` and the Esc-Esc panic-cancel
// gesture.
func (s *Session) RequestAllChildrenCancel(ctx context.Context, reason string) []string {
	s.childMu.Lock()
	ids := make([]string, 0, len(s.children))
	for id := range s.children {
		ids = append(ids, id)
	}
	s.childMu.Unlock()
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if ok, _ := s.RequestChildCancel(ctx, id, reason); ok {
			out = append(out, id)
		}
	}
	return out
}

// RequestChildDismiss is the operator-side counterpart to the
// model-callable `session:subagent_dismiss` tool. Tears down a
// parked child via the standard SessionClose path with reason
// `subagent_dismissed`. Phase 5.2 subagent-lifetime γ.
//
// Return values mirror RequestChildCancel:
//   - (false, ErrCancelEmptyID) — empty childID.
//   - (false, ErrChildNotParked) — child is alive but not in
//     awaiting_dismissal; caller should use RequestChildCancel.
//   - (false, nil) — id is not in the live children map (typo or
//     already torn down). Slash handler surfaces a usage error.
//   - (true, nil) — SessionClose dispatched; teardown observed
//     via the standard SubagentResult projection.
//   - (false, ctx.Err()) — caller's ctx fired mid-dispatch.
func (s *Session) RequestChildDismiss(ctx context.Context, childID string) (bool, error) {
	if strings.TrimSpace(childID) == "" {
		return false, ErrCancelEmptyID
	}
	s.childMu.Lock()
	child, live := s.children[childID]
	s.childMu.Unlock()
	if !live || child == nil || child.IsClosed() {
		return false, nil
	}
	if !childIsParked(child) {
		return false, ErrChildNotParked
	}
	closeFrame := protocol.NewSessionClose(child.id, s.agent.Participant(), dismissCloseReason)
	settled := child.Submit(ctx, closeFrame)
	select {
	case <-settled:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	return true, nil
}

// RequestChildNotify is the operator-side counterpart to the
// model-callable `session:notify_subagent` tool. Delivers a
// directive to a direct child with the same parked-vs-active
// branching as the tool body: parked children get a synthetic
// UserMessage (re-arm), active children get a SystemMessage
// parent_note. Phase 5.2 subagent-lifetime γ.
//
// Returns (rearmed, err):
//   - rearmed=true: child was parked, UserMessage delivered, turn
//     loop will start on the child's next Run iteration.
//   - rearmed=false, err=nil: child was active, parent_note
//     delivered.
//   - err non-nil: child not found, ctx cancelled, or empty id.
func (s *Session) RequestChildNotify(ctx context.Context, childID, content string) (bool, error) {
	if strings.TrimSpace(childID) == "" {
		return false, ErrCancelEmptyID
	}
	if strings.TrimSpace(content) == "" {
		return false, fmt.Errorf("session: notify: content is required")
	}
	s.childMu.Lock()
	child, live := s.children[childID]
	s.childMu.Unlock()
	if !live || child == nil || child.IsClosed() {
		return false, fmt.Errorf("session: notify: %w", ErrCancelEmptyID)
	}
	if childIsParked(child) {
		userMsg := protocol.NewUserMessage(child.ID(), s.agent.Participant(), content)
		settled := child.Submit(ctx, userMsg)
		select {
		case <-settled:
		case <-ctx.Done():
			return false, ctx.Err()
		}
		return true, nil
	}
	frame := protocol.NewSystemMessage(child.ID(), s.agent.Participant(),
		protocol.SystemMessageParentNote, content)
	frame.BaseFrame.FromSession = s.id
	settled := child.Submit(ctx, frame)
	select {
	case <-settled:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	return false, nil
}
