package session

import (
	"context"
	"log/slog"
	"sync"

	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// sessionDeps is the immutable bundle of shared dependencies every
// Session in a Manager-tree references by pointer. It exists so the
// fan-out of root-creation, sub-agent spawn, and boot-time recovery
// can all pass exactly the same set of deps to newSession /
// newSessionRestore without re-deriving them from a Manager pointer
// (Sessions deliberately do not hold a *Manager — exposing the full
// Manager surface to a session would let it create sibling roots
// from inside its own goroutine, undermining the parent-mediated
// isolation phase 4 relies on).
//
// Lifecycle: built once by NewManager from constructor arguments;
// never mutated; shared by every session in the tree (root +
// subagents). Recovery (pkg/session/recover.go) takes a sessionDeps
// directly, no Manager indirection.
//
// rootCtx is the parent ctx for every root session — root.ctx is
// derived from it via context.WithCancelCause. Subagent ctx is
// derived from the parent session's ctx instead, so cancel cascades
// flow naturally through the ctx-chain (ADR
// `phase-4-tree-ctx-routing.md` D7).
//
// wg is shared so Manager.ShutdownAll waits for every goroutine in
// the tree, not just root goroutines.
type sessionDeps struct {
	store     RuntimeStore
	agent     *Agent
	models    *model.ModelRouter
	commands  *CommandRegistry
	codec     *protocol.Codec
	logger    *slog.Logger
	lifecycle Lifecycle
	opts      []SessionOption

	rootCtx context.Context
	wg      *sync.WaitGroup

	// maxDepth is the runtime cap enforced at parent.Spawn(spec)
	// (commit 9 wires this from cfg.Subagents().DefaultMaxDepth();
	// pivot 2 sets a hard-coded default of 5 so existing call sites
	// keep working before the config view lands).
	maxDepth int
}
