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

// RequestChildCancel asks the named direct child to terminate with
// the SessionClose Frame path. Fire-and-forget — unlike the model-
// facing `session:subagent_cancel` tool this does NOT block on
// child.Done(); the parent's pump projects the resulting
// SubagentResult onto the parent's outbox when the child exits.
//
// Used by user-initiated cancel slash commands (phase 5.1c.cancel-ux)
// where the operator's intent is just to send the cancel; the
// asynchronous teardown is observed via the normal lifecycle path.
//
// Idempotent: a child that is already closed (or not in the live
// children map) returns nil. Returns ErrCancelEmptyID when childID
// is empty.
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
	full := protocol.TerminationSubagentCancelPrefix + strings.TrimSpace(reason)
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
