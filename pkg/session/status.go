package session

import (
	"context"

	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

// Lifecycle state machine — emits [protocol.KindSessionStatus]
// frames on every transition so Manager.RestoreActive can classify
// a session's restart behaviour from the persisted event log alone.
//
// States and the live transitions today (foundation; phase-5 HITL
// fills wait_approval / wait_user_input):
//
//	         UserMessage
//	  idle ─────────────────►  active ◄──────────┐
//	   ▲                        │  ▲             │
//	   │  isQuiescent + Final   │  │  result     │
//	   │                        │  │  received   │
//	   │                        ▼  │             │
//	   │            wait_subagents (live)         │
//	   │            wait_approval / wait_user_input
//	   │            (phase-5 placeholder)         │
//	   │                                          │
//	   └──────────────────────────────────────────┘
//
// All emits happen on the session's OWN events log via s.emit. No
// session ever marks status on another session — cross-session
// communication stays frame-based per the runtime constitution.

// Status returns the session's current lifecycle state. Empty
// string means the session never transitioned (pre-newSession).
func (s *Session) Status() string {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	return s.lifecycleState
}

// markStatus is the single transition primitive. Skips the emit
// when the target equals the current state (transition guard) so
// repeated callers (e.g. tools that defer-release back to active)
// don't flood the events log.
//
// reason is a free-form trigger label captured into the marker for
// audit / debugging. Never branched on by runtime code.
//
// Concurrency contract — the persisted events log is the source
// of truth for [Manager.RestoreActive]; the in-memory
// [Session.lifecycleState] mirror is best-effort. We deliberately
// release statusMu BEFORE calling s.emit to avoid holding the
// mutex across a potentially blocking outbox send (s.emit writes
// to a buffered s.out channel; a slow adapter consumer would
// otherwise stall every status transition under contention).
//
// Concurrent callers ARE possible — Run goroutine handles turn
// boundaries / handleSubagentResult / advanceOrFinish; tool
// dispatcher goroutines call markStatus indirectly through
// registerToolFeed and parent.Spawn. Two emits with different
// states can land in the events log under race; the guard
// prevents duplicate identical emits but does not serialise
// distinct state transitions. The classifier reads the newest
// row of the events log, so the worst case is a brief disagreement
// between Status() and the latest persisted state — never a
// crash, never a missed marker. Callers that need a strictly
// consistent read of "what is the lifecycle state right now"
// must consume the events log, not Status().
//
// emit failure logs a warning but does not roll back the in-memory
// state — events remain the source of truth, and a subsequent
// markStatus retry simply emits a fresh marker.
func (s *Session) markStatus(ctx context.Context, state, reason string) {
	s.statusMu.Lock()
	if s.lifecycleState == state {
		s.statusMu.Unlock()
		return
	}
	s.lifecycleState = state
	s.statusMu.Unlock()

	frame := protocol.NewSessionStatus(s.id, s.agent.Participant(), state, reason)
	if err := s.emit(ctx, frame); err != nil && s.logger != nil {
		s.logger.Warn("session: emit lifecycle marker",
			"session", s.id, "state", state, "reason", reason, "err", err)
	}
}

// MarkStatus is the cross-package entry point for the lifecycle
// transition primitive. Used by the supervisor (pkg/session/manager)
// to promote a session out of a stale wait_* state at restore time.
// Internal callers continue to use markStatus directly.
func (s *Session) MarkStatus(ctx context.Context, state, reason string) {
	s.markStatus(ctx, state, reason)
}

// LookupLatestStatusEvent walks events newest-last and returns the
// state of the most recent [protocol.KindSessionStatus] row, or ""
// when the log carries none. Manager.RestoreActive uses the
// store-side narrow probe (LatestEventOfKinds) for the live
// classifier path; this helper survives for tests that build a
// synthetic event slice and want to assert the same lookup
// semantics without round-tripping through the store fixture.
// Reads only — no writes.
func LookupLatestStatusEvent(events []store.EventRow) string {
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if protocol.Kind(ev.EventType) != protocol.KindSessionStatus {
			continue
		}
		if ev.Metadata != nil {
			if v, ok := ev.Metadata["state"].(string); ok && v != "" {
				return v
			}
		}
	}
	return ""
}

// registerToolFeed installs feed as the session's active blocking
// feed and (if feed.BlockingState is non-empty) transitions the
// session to that state. Returns a release closure tools defer to
// drop the feed and transition the session back to active.
//
// Tools that block on inbound frames (wait_subagents today;
// phase-5 HITL approval / clarification) own zero lifecycle code
// — they fill BlockingState declaratively in the ToolFeed and
// the runtime handles every transition.
//
// release is idempotent: a second call is a no-op. The defer
// pattern is therefore safe even when the tool early-returns
// before registering the feed.
func (s *Session) registerToolFeed(ctx context.Context, feed *ToolFeed) (release func()) {
	if feed == nil {
		return func() {}
	}
	s.activeToolFeed.Store(feed)
	if feed.BlockingState != "" {
		s.markStatus(ctx, feed.BlockingState, feed.BlockingReason)
	}
	released := false
	return func() {
		if released {
			return
		}
		released = true
		s.activeToolFeed.Store(nil)
		if feed.BlockingState != "" {
			reason := "released"
			if feed.BlockingReason != "" {
				reason = feed.BlockingReason + " released"
			}
			s.markStatus(ctx, protocol.SessionStatusActive, reason)
		}
	}
}

// isQuiescent returns true when the session has no live work in
// flight — the predicate the Run loop checks at every turn
// boundary to decide whether to mark itself idle.
//
//   - turnState == nil       no active model.Generate goroutine
//   - activeToolFeed == nil  no blocking tool registered
//   - len(children) == 0     no live sub-agents
//   - len(pendingInbound)==0 no buffered frames waiting to drain
//
// Holds childMu briefly to read the children map; the rest are
// loop-goroutine-owned fields read directly. Called only from the
// Run goroutine so turnState / pendingInbound are safe to read
// without a lock.
func (s *Session) isQuiescent() bool {
	if s.turnState != nil {
		return false
	}
	if s.activeToolFeed.Load() != nil {
		return false
	}
	if len(s.pendingInbound) > 0 {
		return false
	}
	s.childMu.Lock()
	hasChild := len(s.children) > 0
	s.childMu.Unlock()
	return !hasChild
}
