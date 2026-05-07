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

// markIdle / markActive / markWaitSubagents are the wired
// transitions today. The phase-5 placeholders below are defined
// for symmetry but never called by runtime code — HITL plumbing
// will start invoking them when it lands.
func (s *Session) markIdle(ctx context.Context, reason string) {
	s.markStatus(ctx, protocol.SessionStatusIdle, reason)
}

func (s *Session) markActive(ctx context.Context, reason string) {
	s.markStatus(ctx, protocol.SessionStatusActive, reason)
}

func (s *Session) markWaitSubagents(ctx context.Context, reason string) {
	s.markStatus(ctx, protocol.SessionStatusWaitSubagents, reason)
}

// markWaitApproval marks the session as paused on a HITL approval
// gate. Phase-5 placeholder — defined for the lifecycle surface
// to be complete, never called by runtime code today.
func (s *Session) markWaitApproval(ctx context.Context, reason string) {
	s.markStatus(ctx, protocol.SessionStatusWaitApproval, reason)
}

// markWaitUserInput marks the session as paused on a HITL
// clarification ask. Phase-5 placeholder — see markWaitApproval.
func (s *Session) markWaitUserInput(ctx context.Context, reason string) {
	s.markStatus(ctx, protocol.SessionStatusWaitUserInput, reason)
}

// lookupLatestStatusEvent walks events newest-last and returns the
// state of the most recent [protocol.KindSessionStatus] row, or ""
// when the log carries none. Used by Manager.RestoreActive to
// classify a session at boot. Reads only — no writes.
func lookupLatestStatusEvent(events []store.EventRow) string {
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
