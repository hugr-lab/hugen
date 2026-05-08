package session

import (
	"context"
	"log/slog"
	"sync"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/store"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// Deps is the immutable bundle of shared dependencies every
// Session in a Manager-tree references by pointer. It exists so the
// fan-out of root-creation, sub-agent spawn, and boot-time recovery
// can all pass exactly the same set of deps to session.New /
// session.NewRestore without re-deriving them from a Manager pointer
// (Sessions deliberately do not hold a *Manager — exposing the full
// Manager surface to a session would let it create sibling roots
// from inside its own goroutine, undermining the parent-mediated
// isolation phase 4 relies on).
//
// Lifecycle: built once by NewManager from constructor arguments;
// never mutated after construction except for the OnCloseRequest
// hook the Manager wires immediately afterwards; shared by every
// session in the tree (root + subagents). Recovery
// (pkg/session/recover.go) takes a Deps directly, no Manager
// indirection.
//
// RootCtx is the parent ctx for every root session — root.ctx is
// derived from it via context.WithCancelCause. Subagent ctx is
// derived from the parent session's ctx instead, so cancel cascades
// flow naturally through the ctx-chain (ADR
// `phase-4-tree-ctx-routing.md` D7).
//
// WG is shared so Manager.Stop waits for every goroutine in
// the tree, not just root goroutines.
//
// Deps is exported so the Manager subpackage (pkg/session/manager)
// can construct one and pass it into session.New / session.NewRestore.
// Outside this module nobody should be assembling a Deps by hand —
// the public surface for that is NewManager.
type Deps struct {
	Store     store.RuntimeStore
	Agent     *Agent
	Models    *model.ModelRouter
	Commands  *CommandRegistry
	Codec     *protocol.Codec
	Tools     *tool.ToolManager
	Logger    *slog.Logger
	Opts      []SessionOption

	// Extensions is the agent-level set of registered extensions.
	// NewSession iterates this list and dispatches each extension to
	// the capability hooks it implements (StateInitializer at open,
	// Recovery at materialise, Closer at teardown, …). Order is
	// preserved: Advertisers contribute prompt sections in this
	// order; Closers are called in reverse.
	Extensions []extension.Extension

	RootCtx context.Context
	WG      *sync.WaitGroup

	// MaxDepth is the runtime cap enforced at parent.Spawn(spec)
	// (commit 9 wires this from cfg.Subagents().DefaultMaxDepth();
	// pivot 2 sets a hard-coded default of 5 so existing call sites
	// keep working before the config view lands). Constructors
	// without a configured value fall back to DefaultMaxDepth.
	MaxDepth int

	// OnCloseRequest is the optional outbound hook a root session
	// fires from requestClose (phase-4.1b-pre stage B / D6). The hook
	// hands the close request to whoever owns the session's
	// termination — Manager populates it with a goroutine that calls
	// Manager.Terminate, which Submits SessionClose back to the root.
	// Subagents do NOT consult this hook; they emit subagent_result
	// to their parent and idle until the parent issues SessionClose.
	// Tests with no Manager wiring drive teardown via the SessionClose
	// Frame directly.
	OnCloseRequest func(ctx context.Context, sessionID, reason string)
}

// DefaultMaxDepth is the phase-4 fallback for Deps.MaxDepth until
// commit 9 wires cfg.Subagents().DefaultMaxDepth. Matches
// `phase-4-spec.md §5.7` Layer 2 default. Exported so the manager
// subpackage can populate Deps.MaxDepth at NewManager time without
// duplicating the constant.
const DefaultMaxDepth = 5
