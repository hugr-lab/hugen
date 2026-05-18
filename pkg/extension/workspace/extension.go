// Package workspace owns the per-session scratch directory both
// as a runtime-level tracker (the [tracker] type) and as a session
// [extension.Extension]. Phase 5.4 made the layout mission-aware:
//
//   - chat root (depth 0) writes under WORKSPACES_ROOT/<root_id>/
//   - mission (depth 1) writes under WORKSPACES_ROOT/<root_id>/<mission_id>/
//   - worker (depth ≥ 2) inherits the nearest mission ancestor's dir
//     verbatim — every worker in the same mission shares one
//     workspace so wave outputs hand off via $SESSION_DIR without
//     extra plumbing.
//
// CloseSession is forget-only at every tier. Filesystem reclamation
// is the orphan sweeper's job for mission subdirs (TTL-based) and
// phase-6 cron's job for root dirs (chat-session retention policy).
package workspace

import (
	"context"
	"log/slog"
	"path/filepath"

	"github.com/hugr-lab/hugen/pkg/extension"
)

// StateKey is the [extension.SessionState] key the extension
// stashes its [*SessionWorkspace] handle under.
const StateKey = "workspace"

const extensionName = "workspace"

// Tier identifies the role a session plays in the spawn tree.
// Workspace layout and lifetime rules branch on this.
type Tier int

const (
	// TierRoot — depth 0, chat session. Workspace path is
	// <WORKSPACES_ROOT>/<session_id>/. Persists past close; cleanup
	// is deferred to phase-6 cron.
	TierRoot Tier = 0
	// TierMission — depth 1, mission dispatcher. Workspace path is
	// <WORKSPACES_ROOT>/<root_id>/<session_id>/. Persists past
	// close; reclaimed by the orphan sweeper after TTL.
	TierMission Tier = 1
	// TierWorker — depth ≥ 2. Workspace path is the nearest mission
	// ancestor's dir, shared with siblings in the same mission.
	// Worker close is a no-op on disk.
	TierWorker Tier = 2
)

// Extension is the agent-level singleton owning the workspace
// tracker. Constructed once at runtime boot from the workspace
// root; per-session state lives on [*SessionWorkspace] handles
// keyed under [StateKey].
type Extension struct {
	tracker *tracker
	logger  *slog.Logger
}

// NewExtension constructs the workspace extension. root is the
// absolute path under which the layout above is materialised
// (mkdir -p as needed); logger is the agent-level slog handle
// CloseSession uses to record mission-dir paths at INFO so
// operators can find them after the session terminated (nil falls
// back to slog.Default).
//
// Phase 5.4 dropped the cleanup-on-close flag: dir lifetime is
// uniformly owned by the orphan sweeper (mission subdirs) and
// phase-6 cron (root dirs).
func NewExtension(root string, logger *slog.Logger) *Extension {
	if logger == nil {
		logger = slog.Default()
	}
	return &Extension{tracker: newTracker(root), logger: logger}
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
// shared workspace root + the session's tier so other extensions
// (mcp, future providers) can read paths and lifetime contracts
// without reaching for the tracker directly.
type SessionWorkspace struct {
	dir  string
	root string
	tier Tier
}

// Dir returns the absolute path of this session's workspace
// directory. For workers, this is the mission ancestor's dir
// (shared with siblings). Always populated by InitState; only ever
// empty when the extension was registered without a tracker.
func (h *SessionWorkspace) Dir() string { return h.dir }

// Root returns the absolute path of the workspace root every
// session shares (typically WORKSPACES_ROOT in spawned MCP envs).
func (h *SessionWorkspace) Root() string { return h.root }

// Tier reports the session's role in the spawn tree (root /
// mission / worker). Tools that care about lifetime semantics can
// branch on this rather than re-deriving from Depth.
func (h *SessionWorkspace) Tier() Tier { return h.tier }

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
// per-session directory according to the session's depth in the
// spawn tree (see the [Tier] constants for the path scheme) and
// stashes the [*SessionWorkspace] handle on state.SetValue keyed
// by [StateKey]. Order matters: the workspace extension must
// register before any extension that reads workspace paths
// (mcp's per_session spawn, ...) so its state is visible by the
// time the dependent's InitState runs.
func (e *Extension) InitState(_ context.Context, state extension.SessionState) error {
	if e.tracker == nil {
		return nil
	}
	root, err := e.tracker.Root()
	if err != nil {
		return err
	}

	depth := state.Depth()
	switch {
	case depth <= 0:
		// Chat root. Flat under workspaces root, keyed by session id.
		dir, err := e.tracker.acquireAt(state.SessionID(), state.SessionID())
		if err != nil {
			return err
		}
		state.SetValue(StateKey, &SessionWorkspace{dir: dir, root: root, tier: TierRoot})

	case depth == 1:
		// Mission. Nest under <root_id>/<mission_id>/. Walk up one
		// parent (the root) to learn its id; fall back to using the
		// mission's own id at the top level if the parent linkage is
		// missing (test fixture without a parent, never a runtime
		// path).
		rootID := state.SessionID()
		if anc := walkUpToDepth(state, 0); anc != nil {
			rootID = anc.SessionID()
		}
		relPath := filepath.Join(rootID, state.SessionID())
		dir, err := e.tracker.acquireAt(state.SessionID(), relPath)
		if err != nil {
			return err
		}
		state.SetValue(StateKey, &SessionWorkspace{dir: dir, root: root, tier: TierMission})

	default:
		// Worker (depth ≥ 2 — direct child of mission, or a deeper
		// can_spawn grandchild). Inherit the nearest mission
		// ancestor's dir verbatim — no acquireAt call so there is
		// nothing to forget on close.
		var inherited *SessionWorkspace
		if anc := walkUpToDepth(state, 1); anc != nil {
			inherited = FromState(anc)
		}
		if inherited == nil {
			// Defensive fallback: no mission ancestor reachable. Not
			// a real runtime path today (root → worker isn't a
			// supported spawn). Use a flat dir keyed by session id
			// so the worker still has somewhere to write.
			dir, err := e.tracker.acquireAt(state.SessionID(), state.SessionID())
			if err != nil {
				return err
			}
			state.SetValue(StateKey, &SessionWorkspace{dir: dir, root: root, tier: TierWorker})
			return nil
		}
		state.SetValue(StateKey, &SessionWorkspace{
			dir:  inherited.Dir(),
			root: inherited.Root(),
			tier: TierWorker,
		})
	}
	return nil
}

// walkUpToDepth returns the ancestor whose Depth() equals target,
// or the topmost reachable ancestor if target isn't reached.
// Returns nil only when s itself is nil.
func walkUpToDepth(s extension.SessionState, target int) extension.SessionState {
	for s != nil && s.Depth() > target {
		p, ok := s.Parent()
		if !ok {
			return s
		}
		s = p
	}
	return s
}

// CloseSession implements [extension.Closer]. Forgets the
// session's tracker entry without touching the filesystem.
// Reclamation is handled out-of-band: mission subdirs by the
// orphan sweeper (TTL-based), root dirs by phase-6 cron.
//
// At INFO the mission's dir path is logged on close so operators
// debugging a finished pipeline can find the artefacts it left
// behind without recovering the spawn graph from session_events.
// Root and worker close are not logged (root path is trivially
// derivable from session id; workers don't own a dir).
func (e *Extension) CloseSession(_ context.Context, state extension.SessionState) error {
	if e.tracker == nil {
		return nil
	}
	dir := e.tracker.forget(state.SessionID())
	if h := FromState(state); h != nil && h.Tier() == TierMission && dir != "" {
		e.logger.Info("workspace: mission session closed",
			"session_id", state.SessionID(),
			"dir", dir)
	}
	return nil
}
