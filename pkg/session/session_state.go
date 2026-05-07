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

// Emit implements [extension.SessionState]. Persists frame to the
// session's event log and pushes it through the outbox; the
// internal lowercase emit holds the actual logic (next-seq +
// store.AppendEvent + outbox push).
func (s *Session) Emit(ctx context.Context, frame protocol.Frame) error {
	return s.emit(ctx, frame)
}
