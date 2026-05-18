package perm

import "context"

// SessionContext carries the per-call session facts that
// PermissionService needs to render templates inside a Permission's
// Data and Filter values. AgentID and Role are agent-stable and
// come from the constructor (identity.Source); SessionID and
// SessionMetadata vary per call and flow through context.
//
// WorkspaceDir is the absolute path of the calling session's
// workspace as resolved by the workspace extension (5.4 layout:
// root dirs at top, mission dirs nested under root, workers
// inheriting their mission ancestor). Per_agent MCP providers
// like hugr-query / python-mcp use this — passed through MCP
// `_meta.session_dir` — to route file output into the right
// shared mission folder instead of computing a flat
// `<workspace_root>/<session_id>/` path. Empty when the dispatch
// site has no workspace bound (test fixtures, one-off /skill list,
// pre-extension callers).
type SessionContext struct {
	SessionID       string
	SessionMetadata map[string]string
	WorkspaceDir    string
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
