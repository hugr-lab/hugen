package extension

import (
	"context"

	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// SessionState is the typed-handle bag every per-session
// extension and tool provider reaches into. Implementations
// (today: *session.Session, test fakes) own the underlying
// concurrency; callers just SetValue / Value through the
// interface.
//
// Extensions store per-session projections under a stable name
// (typically the extension's own [Extension.Name]). Sub-agents
// reach the host's state through [SessionState.Parent] — call
// SessionID / Value on the returned parent directly.
//
// Tools returns the per-session child [*tool.ToolManager] —
// extensions that need to mount providers dynamically at runtime
// (skill_load reading a manifest's allowed-tools and registering
// the matching MCP) call AddProvider on the result. The returned
// manager's lifetime is the session's; closing it is the
// session's job, not the caller's.
type SessionState interface {
	SessionID() string
	Value(name string) (any, bool)
	SetValue(name string, value any)

	// Parent returns the parent session's state for sub-agents and
	// (nil, false) for root sessions. Callers read parent's
	// SessionID / Value directly off the returned handle —
	// transparently traverses any depth.
	Parent() (SessionState, bool)

	Tools() *tool.ToolManager

	// WorkspaceDir returns the absolute path of this session's
	// workspace directory and ok=true once the lifecycle has
	// acquired it (always before [StateInitializer.InitState]
	// runs). Returns ("", false) for sessions whose runtime has
	// no workspace wired (test fixtures).
	WorkspaceDir() (string, bool)

	// WorkspaceRoot returns the absolute path of the workspace
	// root every session shares, and ok=true when the runtime has
	// a workspace wired. Extensions that need to expose the root
	// to their providers (e.g. MCP env WORKSPACES_ROOT) read it
	// here.
	WorkspaceRoot() (string, bool)

	// Emit persists frame on the calling session's event log and
	// pushes it through the session's outbox for adapters.
	// Extensions emitting state-change events ([protocol.ExtensionFrame]
	// with Category=Op) call this so Recovery can replay them on
	// restart.
	Emit(ctx context.Context, frame protocol.Frame) error
}

type sessionStateKey struct{}

// WithSessionState attaches state to ctx. session.Session calls
// this before tool dispatch so handlers downstream can recover
// the calling session's typed state.
func WithSessionState(ctx context.Context, state SessionState) context.Context {
	return context.WithValue(ctx, sessionStateKey{}, state)
}

// SessionStateFromContext returns the state attached via
// [WithSessionState], or (nil, false) if no state is present.
// Tool providers and extension handlers use this to recover the
// calling session's typed state from the dispatch ctx.
func SessionStateFromContext(ctx context.Context) (SessionState, bool) {
	s, ok := ctx.Value(sessionStateKey{}).(SessionState)
	return s, ok
}
