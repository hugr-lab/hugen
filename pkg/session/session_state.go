package session

import "github.com/hugr-lab/hugen/pkg/tool"

var _ (tool.SessionState) = (*Session)(nil)

func (s *Session) ParentID() string {
	if s.parent == nil {
		return ""
	}
	return s.parent.SessionID()
}

// ParentValue implements [tool.SessionState].
func (s *Session) ParentValue(name string) (any, bool) {
	if s.parent == nil {
		return nil, false
	}
	return s.parent.Value(name)
}

// SessionID implements [tool.SessionState].
func (s *Session) SessionID() string {
	return s.id
}

// SetValue implements [tool.SessionState].
func (s *Session) SetValue(name string, value any) {
	panic("unimplemented")
}

// Value implements [tool.SessionState].
func (s *Session) Value(name string) (any, bool) {
	panic("unimplemented")
}
