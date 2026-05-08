// Package workspace owns the per-session scratch directory both
// as a runtime-level tracker (the [Workspace] type) and as a
// session [extension.Extension]. Stage 5c moved this off
// pkg/session.Lifecycle: the extension's InitState acquires the
// session's directory (mkdir under the tracker's root), stores a
// [*SessionWorkspace] handle on [extension.SessionState], and
// CloseSession releases the dir (delete-on-close when the tracker
// was constructed with cleanup=true). MCP / future per-session
// extensions read paths via [FromState].
package workspace

import (
	"context"

	"github.com/hugr-lab/hugen/pkg/extension"
)

// StateKey is the [extension.SessionState] key the extension
// stashes its [*SessionWorkspace] handle under.
const StateKey = "workspace"

const extensionName = "workspace"

// Extension is the agent-level singleton owning the session-
// directory tracker. Constructed once at runtime boot from
// (root, cleanup); per-session state lives on
// [*SessionWorkspace] handles.
type Extension struct {
	tracker *tracker
}

// NewExtension constructs the workspace extension. root is the
// absolute path under which per-session directories are created
// (mkdir -p as needed); cleanup decides whether CloseSession
// deletes the directory or just forgets it (operators debugging
// crashed sessions sometimes prefer dirs to linger).
func NewExtension(root string, cleanup bool) *Extension {
	return &Extension{tracker: newTracker(root, cleanup)}
}

// Root returns the absolute workspace root the extension was
// configured with. Exported so tool builders that need the path
// at agent-boot time (providers.Builder for the
// allowed-host-mounts list, etc.) can read it without
// instantiating a session.
func (e *Extension) Root() (string, error) {
	if e.tracker == nil {
		return "", nil
	}
	return e.tracker.Root()
}

// Compile-time interface assertions.
var (
	_ extension.Extension        = (*Extension)(nil)
	_ extension.StateInitializer = (*Extension)(nil)
	_ extension.Closer           = (*Extension)(nil)
)

// Name implements [extension.Extension].
func (e *Extension) Name() string { return extensionName }

// SessionWorkspace is the per-session typed handle stored under
// [StateKey]. It exposes the absolute session directory + the
// shared workspace root so other extensions (mcp, future
// providers) can read paths without reaching for the tracker
// directly.
type SessionWorkspace struct {
	dir  string
	root string
}

// Dir returns the absolute path of this session's workspace
// directory. Always populated by InitState; only ever empty when
// the extension was registered without a Workspace tracker.
func (h *SessionWorkspace) Dir() string { return h.dir }

// Root returns the absolute path of the workspace root every
// session shares (typically WORKSPACES_ROOT in spawned MCP envs).
func (h *SessionWorkspace) Root() string { return h.root }

// FromState returns the per-session [*SessionWorkspace] handle,
// or nil when the extension hasn't run InitState (test fixtures
// without the workspace extension wired).
func FromState(state extension.SessionState) *SessionWorkspace {
	if state == nil {
		return nil
	}
	v, ok := state.Value(StateKey)
	if !ok {
		return nil
	}
	h, _ := v.(*SessionWorkspace)
	return h
}

// InitState implements [extension.StateInitializer]. Acquires the
// per-session directory via the tracker (creates it on disk) and
// stashes the [*SessionWorkspace] handle on state.SetValue
// keyed by [StateKey]. Order matters: the workspace extension
// must register before any extension that reads workspace paths
// (mcp's per_session spawn, ...) so its state is visible by the
// time the dependent's InitState runs.
func (e *Extension) InitState(_ context.Context, state extension.SessionState) error {
	if e.tracker == nil {
		return nil
	}
	dir, err := e.tracker.acquire(state.SessionID())
	if err != nil {
		return err
	}
	root, err := e.tracker.Root()
	if err != nil {
		return err
	}
	state.SetValue(StateKey, &SessionWorkspace{dir: dir, root: root})
	return nil
}

// CloseSession implements [extension.Closer]. Releases the
// session's directory entry on the tracker; with cleanup=true
// (the default for production deployments) the directory is
// removed too. Idempotent — release on an unknown session is a
// no-op.
func (e *Extension) CloseSession(_ context.Context, state extension.SessionState) error {
	if e.tracker == nil {
		return nil
	}
	return e.tracker.release(state.SessionID())
}
