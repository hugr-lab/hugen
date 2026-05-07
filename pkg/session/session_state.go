package session

import "github.com/hugr-lab/hugen/pkg/tool"

// Session implements [tool.SessionState] — extensions and tool
// providers stash per-session typed handles via SetValue and read
// them via Value. Keys are extension names; values are
// extension-defined typed handles whose concurrency is the
// handle's own concern.

var _ (tool.SessionState) = (*Session)(nil)

// SessionID implements [tool.SessionState].
func (s *Session) SessionID() string { return s.id }

// ParentID implements [tool.SessionState]. Returns "" for root
// sessions.
func (s *Session) ParentID() string {
	if s.parent == nil {
		return ""
	}
	return s.parent.id
}

// SetValue implements [tool.SessionState]. Stores a typed handle
// under name; subsequent Value(name) returns it. Idempotent —
// repeated SetValue overwrites.
func (s *Session) SetValue(name string, value any) {
	s.state.Store(name, value)
}

// Value implements [tool.SessionState]. Returns (handle, true) if
// SetValue stored anything under name on this session, (nil, false)
// otherwise. Does NOT walk up to the parent — see ParentValue.
func (s *Session) Value(name string) (any, bool) {
	return s.state.Load(name)
}

// ParentValue implements [tool.SessionState]. Reads the parent
// session's Value(name) — sub-agents inspecting host state
// (whiteboard host projection, parent's loaded skills) use this.
// Returns (nil, false) for root sessions.
func (s *Session) ParentValue(name string) (any, bool) {
	if s.parent == nil {
		return nil, false
	}
	return s.parent.Value(name)
}
