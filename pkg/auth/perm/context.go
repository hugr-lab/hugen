package perm

import "context"

// SessionContext carries the per-call session facts that
// PermissionService needs to render templates inside a Permission's
// Data and Filter values. AgentID and Role are agent-stable and
// come from the constructor (identity.Source); SessionID and
// SessionMetadata vary per call and flow through context.
type SessionContext struct {
	SessionID       string
	SessionMetadata map[string]string
}

type sessionCtxKey struct{}

// WithSession attaches sc to ctx so the permission service can
// pick it up inside Resolve. ToolManager (and any other caller of
// perm.Service.Resolve) is expected to call WithSession before
// dispatch; an absent SessionContext substitutes empty values for
// [$session.*] template tokens, which is correct for one-off
// out-of-session callers like /skill list.
func WithSession(ctx context.Context, sc SessionContext) context.Context {
	return context.WithValue(ctx, sessionCtxKey{}, sc)
}

// SessionFromContext returns the SessionContext attached by
// WithSession or the zero value (and false) when none is present.
func SessionFromContext(ctx context.Context) (SessionContext, bool) {
	sc, ok := ctx.Value(sessionCtxKey{}).(SessionContext)
	return sc, ok
}
