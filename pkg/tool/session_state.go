package tool

import "context"

type SessionState interface {
	SessionID() string
	Value(name string) (any, bool)
	SetValue(name string, value any)

	ParentID() string
	ParentValue(name string) (any, bool)
}

type sessionStateKey struct{}

func WithSessionState(ctx context.Context, state SessionState) context.Context {
	return context.WithValue(ctx, sessionStateKey{}, state)
}

func SessionStateFromContext(ctx context.Context) (SessionState, bool) {
	s, ok := ctx.Value(sessionStateKey{}).(SessionState)
	return s, ok
}
