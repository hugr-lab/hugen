package session

import (
	"context"
	"errors"
	"strings"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// ErrCancelEmptyID is returned by RequestChildCancel when the caller
// passes an empty session id.
var ErrCancelEmptyID = errors.New("session: cancel: child id is required")

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
func (s *Session) RequestChildCancel(ctx context.Context, childID, reason string) error {
	if strings.TrimSpace(childID) == "" {
		return ErrCancelEmptyID
	}
	s.childMu.Lock()
	child, live := s.children[childID]
	s.childMu.Unlock()
	if !live || child.IsClosed() {
		return nil
	}
	r := strings.TrimSpace(reason)
	full := protocol.TerminationUserCancelPrefix + r
	// Step 1: Cancel with cascade — aborts the child's in-flight
	// turn and cascade-closes its workers.
	cancelFrame := protocol.NewCancel(child.id, s.agent.Participant(), full)
	cancelFrame.Payload.Cascade = true
	child.Submit(ctx, cancelFrame)
	// Step 2: SessionClose — terminates the child itself with a
	// skip-close-turn reason so teardown does not block on a
	// findings model turn the operator did not ask for.
	closeFrame := protocol.NewSessionClose(child.id, s.agent.Participant(), full)
	child.Submit(ctx, closeFrame)
	return nil
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
		if err := s.RequestChildCancel(ctx, id, reason); err == nil {
			out = append(out, id)
		}
	}
	return out
}
