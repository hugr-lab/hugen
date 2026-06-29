package runtime

import (
	"context"
	"log/slog"
	stdhttp "net/http"
	"sync"

	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/config"
	"github.com/hugr-lab/hugen/pkg/extension"
	artifactext "github.com/hugr-lab/hugen/pkg/extension/artifact"
	"github.com/hugr-lab/hugen/pkg/identity"
	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/prompts"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/scheduler/runner"
	schedstore "github.com/hugr-lab/hugen/pkg/scheduler/store"
	"github.com/hugr-lab/hugen/pkg/session"
	"github.com/hugr-lab/hugen/pkg/session/manager"
	"github.com/hugr-lab/hugen/pkg/skill"
	"github.com/hugr-lab/hugen/pkg/tool"
	"github.com/hugr-lab/hugen/pkg/tool/providers/policies"

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

	// Prompts is the agent-level template renderer. It depends only on
	// the embedded templates + Logger, so it has no phase-ordering
	// constraint — built lazily by [Core.PromptRenderer] on first use
	// (phaseExtensions for the compactor, phaseSessionManager for
	// sessions) and shared. Inject agent-level constants like this via
	// constructor; per-session state still flows through SessionState.
	Prompts *prompts.Renderer

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

	// Phase 6.1b (scheduler storage). Backs TaskManager extension
	// + task_log_reap_stuck system runner. Constructed in
	// phaseStorage alongside Store.
	TaskStore schedstore.TaskStore

	// Phase 5 (models).
	Models *model.ModelRouter

	// Phase 6 (agent).
	Agent    *session.Agent
	Commands *session.CommandRegistry
	Codec    *protocol.Codec

	// Phase 7 (skills_perms).
	Skills      *skill.SkillManager
	SkillStore  skill.SkillStore
	Permissions perm.Service

	// Phase 8 (tools).
	Tools    *tool.ToolManager
	Policies *policies.Policies

	// Phase 8.5 (extensions). Built by phaseExtensions; consumed by
	// phaseSessionManager via session.WithExtensions. Each extension
	// implementing tool.ToolProvider is also registered on Tools so
	// its catalogue surfaces to Snapshot/Resolve/Dispatch.
	Extensions []extension.Extension

	// Phase 8 (artifacts). The durable user-facing artifact store
	// extension. Stashed so phaseRunner can register the idle reaper
	// over the same store and adapters can reach Ingest (upload) /
	// Store().Path (download). Constructed in phaseExtensions.
	Artifacts *artifactext.Extension

	// Phase 9 (session_manager).
	Manager *manager.Manager

	// Phase 10 (runner). Agent-level scheduling primitive that
	// dispatches always-on resilience reapers today (§16.1) and
	// per-session TaskManager fire fns once Phase 6.1b ships.
	// Constructed + started by phaseRunner; stopped via the
	// cleanup stack ahead of Manager.Stop so in-flight reapers
	// drain before the session manager unwinds.
	Runner runner.Runner

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
