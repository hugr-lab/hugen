package session

import (
	"context"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// Session implements [extension.SessionState] — extensions and tool
// providers stash per-session typed handles via SetValue and read
// them via Value. Keys are extension names; values are
// extension-defined typed handles whose concurrency is the
// handle's own concern.

var _ (extension.SessionState) = (*Session)(nil)

// SessionID implements [extension.SessionState].
func (s *Session) SessionID() string { return s.id }

// SetValue implements [extension.SessionState]. Stores a typed
// handle under name; subsequent Value(name) returns it.
// Idempotent — repeated SetValue overwrites.
func (s *Session) SetValue(name string, value any) {
	s.state.Store(name, value)
}

// Value implements [extension.SessionState]. Returns (handle, true)
// if SetValue stored anything under name on this session,
// (nil, false) otherwise. Does NOT walk up to the parent — call
// Parent() and read its Value directly for that.
func (s *Session) Value(name string) (any, bool) {
	return s.state.Load(name)
}

// Parent implements [extension.SessionState]. Returns the parent
// session's state for sub-agents and (nil, false) for root
// sessions.
func (s *Session) Parent() (extension.SessionState, bool) {
	if s.parent == nil {
		return nil, false
	}
	return s.parent, true
}

// Children implements [extension.SessionState]. Returns a
// snapshot of every direct child's state, or nil when the
// session has no children. Safe for the caller to iterate
// without holding any session lock; mutations after the call
// are not reflected.
func (s *Session) Children() []extension.SessionState {
	s.childMu.Lock()
	defer s.childMu.Unlock()
	if len(s.children) == 0 {
		return nil
	}
	out := make([]extension.SessionState, 0, len(s.children))
	for _, c := range s.children {
		if c == nil {
			continue
		}
		out = append(out, c)
	}
	return out
}

// Emit implements [extension.SessionState]. Persists frame to the
// session's event log and pushes it through the outbox; the
// internal lowercase emit holds the actual logic (next-seq +
// store.AppendEvent + outbox push).
func (s *Session) Emit(ctx context.Context, frame protocol.Frame) error {
	return s.emit(ctx, frame)
}

// OutboxOnly implements [extension.SessionState]. Publishes frame
// on the session's outbox without persisting to the event log
// (no AppendEvent, no seq allocation). Same wire shape as Emit
// for live subscribers; absent from any post-mortem replay. Used
// by extensions producing transient observability frames
// (liveview status updates, future heartbeat-style traffic).
// Phase 5.1b.
func (s *Session) OutboxOnly(ctx context.Context, frame protocol.Frame) error {
	return s.outboxOnly(ctx, frame)
}

// Extensions implements [extension.SessionState]. Returns the
// agent-level extension slice in registration order. Aggregator
// extensions (notably liveview) iterate and type-assert
// capabilities like [extension.StatusReporter] without hardcoding
// extension names. Returns nil when the session was constructed
// without deps. Phase 5.1b.
func (s *Session) Extensions() []extension.Extension {
	if s.deps == nil {
		return nil
	}
	return s.deps.Extensions
}
