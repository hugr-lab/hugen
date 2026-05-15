package session

import (
	"context"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// resolveChildAutoclose walks the parent's deps.Extensions for the
// first [extension.AutocloseLookup] that returns a definitive answer
// for the child's (spawnSkill, spawnRole) pair. Returns true (the
// legacy auto-close path) when no extension speaks up — preserves
// status quo for sessions with no SkillManager wired (tests / no-
// skill deployments) and for children whose dispatching skill is
// no longer loaded on the parent.
//
// Phase 5.2 subagent-lifetime β.
func (s *Session) resolveChildAutoclose(ctx context.Context, child *Session) bool {
	if s.deps == nil || child == nil {
		return true
	}
	for _, ext := range s.deps.Extensions {
		l, ok := ext.(extension.AutocloseLookup)
		if !ok {
			continue
		}
		if val, found := l.ResolveAutoclose(ctx, s, child.spawnSkill, child.spawnRole); found {
			return val
		}
	}
	return true
}

// parkChild flips a child into the awaiting_dismissal lifecycle
// state instead of closing it. The child's Run loop stays alive
// (idle, waiting on its inbound channel) so a later
// session:notify_subagent can re-arm it OR a session:subagent_dismiss
// can tear it down via the existing SessionClose path.
//
// Called from the parent's [handleSubagentResult] when the child
// terminated normally (Reason == TerminationCompleted) AND the
// resolver chain produced autoclose=false. Any non-success reason
// (cancel, hard ceiling, abnormal close, panic, model error) keeps
// the legacy auto-close path: parking a broken / cancelled child
// would only waste the slot.
//
// Phase 5.2 subagent-lifetime β.
func (s *Session) parkChild(ctx context.Context, child *Session) {
	if child == nil {
		return
	}
	// markStatus is idempotent: a repeated park (e.g. observer
	// race) emits no second status frame.
	child.markStatus(ctx, protocol.SessionStatusAwaitingDismissal, "parked_on_result")
}

// childIsParked reports whether the given child is currently in
// the awaiting_dismissal lifecycle state. Used by routing /
// isQuiescent to skip parked children from "live work" counts.
func childIsParked(c *Session) bool {
	if c == nil {
		return false
	}
	return c.Status() == protocol.SessionStatusAwaitingDismissal
}
