package runtime

import (
	"context"
	"log/slog"
	stdhttp "net/http"
	"sync"

	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/hugr-lab/hugen/pkg/config"
	"github.com/hugr-lab/hugen/pkg/identity"
	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session"

	hugr "github.com/hugr-lab/query-engine"
	"github.com/hugr-lab/query-engine/client"
	"github.com/hugr-lab/query-engine/types"
)

// Core aggregates every dependency a subcommand handler needs.
// Constructed once by Build; fields are read-only afterwards
// (constitution principle II — no Set* mutators). Phases populate
// fields incrementally; later phases read what earlier ones wrote.
//
// During phase 4.1a Stage B-E the field set grows as helpers move
// out of cmd/hugen. The skeleton keeps only the boot-time fields
// every phase needs (Cfg, Logger) plus Shutdown plumbing.
type Core struct {
	Cfg    Config
	Logger *slog.Logger

	// Phase 2 (http_auth).
	HTTPSrv *stdhttp.Server
	Mux     *stdhttp.ServeMux
	Auth    *auth.Service

	// Phase 3 (identity).
	RemoteQuerier *client.Client
	Identity      identity.Source

	// Phase 4 (storage).
	Config       *config.StaticService
	LocalEngine  *hugr.Service
	LocalQuerier types.Querier
	Store        session.RuntimeStore

	// Phase 5 (models).
	Models *model.ModelRouter

	// Phase 6 (agent).
	Agent    *session.Agent
	Commands *session.CommandRegistry
	Codec    *protocol.Codec

	// cleanups stacks per-phase teardown closures in registration
	// order. cleanupPartial (failure path) and Shutdown (success
	// path) iterate it in reverse so resources unwind in the
	// reverse of construction order. Phases append via
	// core.addCleanup(fn) as they acquire owned resources.
	cleanups []func()

	shutdownOnce sync.Once
}

// addCleanup registers a teardown closure. Phases call it after
// acquiring a resource that needs explicit close on shutdown or
// partial-build failure.
func (c *Core) addCleanup(fn func()) {
	if fn == nil {
		return
	}
	c.cleanups = append(c.cleanups, fn)
}

// cleanupPartial unwinds every registered cleanup in reverse order.
// Called from Build's failed() helper on a phase error; idempotent
// because each phase guards its own resource acquisition.
func (c *Core) cleanupPartial() {
	for i := len(c.cleanups) - 1; i >= 0; i-- {
		c.cleanups[i]()
	}
	c.cleanups = nil
}

// Shutdown runs every registered cleanup once. Safe to call
// multiple times; subsequent calls are no-ops.
//
// The ctx argument is reserved for phase-specific timeouts (e.g.
// HTTP server graceful shutdown, session persistence) once Stage E
// wires phase 9. The skeleton ignores it.
func (c *Core) Shutdown(ctx context.Context) {
	_ = ctx
	c.shutdownOnce.Do(func() {
		c.cleanupPartial()
	})
}
