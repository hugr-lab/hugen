package main

import (
	"context"
	"fmt"
	"log/slog"
	stdhttp "net/http"
	"sync"
	"time"

	hugr "github.com/hugr-lab/query-engine"
	"github.com/hugr-lab/query-engine/client"
	"github.com/hugr-lab/query-engine/types"

	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/config"
	"github.com/hugr-lab/hugen/pkg/identity"
	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/models"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session"
	"github.com/hugr-lab/hugen/pkg/skill"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// RuntimeCore aggregates every dependency a subcommand handler needs.
// Constructed once by buildRuntimeCore at process start; fields are
// read-only afterwards (constitution principle II — no Set* mutators).
//
// See specs/002-agent-runtime-phase-2/contracts/runtime-core.md for
// the full contract.
type RuntimeCore struct {
	Boot *BootstrapConfig
	// Config is the phase-3 per-domain views aggregate. All
	// downstream packages (pkg/runtime, pkg/auth/perm, pkg/skill,
	// pkg/tool, pkg/store/local) take narrow Views through it.
	Config   *config.StaticService
	Logger   *slog.Logger
	Auth     *auth.Service
	Identity identity.Source
	Agent    *session.Agent
	Models   *model.ModelRouter
	Manager  *session.Manager
	Commands *session.CommandRegistry
	Codec    *protocol.Codec

	// Phase-3 stack: skills + permissions + tools.
	Skills      *skill.SkillManager
	SkillStore  skill.SkillStore
	Permissions perm.Service
	Tools       *tool.ToolManager
	workspaces  *sessionWorkspaces

	// HTTPSrv hosts the auth endpoints (phase 1) and, in phase 2,
	// /api/v1/* via pkg/adapter/http. Both share the same mux so the
	// existing bearer middleware applies uniformly.
	HTTPSrv *stdhttp.Server
	Mux     *stdhttp.ServeMux

	// Stored so cmd-level helpers and adapters can reuse them.
	// LocalEngine is the typed handle owning the embedded DuckDB
	// connection — held alongside LocalQuerier so Shutdown can call
	// LocalEngine.Close() without an io.Closer round-trip.
	LocalEngine   *hugr.Service
	LocalQuerier  types.Querier
	RemoteQuerier *client.Client

	// Store is the persistence facade backing the SessionManager.
	// Held on the core so adapters that need replay (the http
	// adapter for Last-Event-ID resume) can reach it without
	// reaching into the manager.
	Store session.RuntimeStore

	shutdownOnce sync.Once
}

// buildRuntimeCore brings up every dependency the agent needs at boot.
// It blocks long enough to: load config, start the auth HTTP listener,
// resolve identity (which may involve a network round-trip in
// personal-assistant mode), open the local store, and assemble the
// session manager.
//
// On error every partially-constructed resource is cleaned up before
// returning; the caller is not responsible for cleanup of a failed
// boot. On success the caller MUST defer Shutdown(ctx).
func buildRuntimeCore(ctx context.Context) (*RuntimeCore, error) {
	core := &RuntimeCore{}

	// failed wraps an in-flight error and runs the cleanup that
	// reverses every step of the build that has run so far. Used as
	// `return nil, failed("step", err)` from the partial-failure
	// branches below; keeps the per-step boilerplate to one line.
	failed := func(step string, err error) error {
		core.cleanupPartial()
		return fmt.Errorf("buildRuntimeCore: %s: %w", step, err)
	}

	boot, err := loadBootstrapConfig(".env")
	if err != nil {
		return nil, fmt.Errorf("buildRuntimeCore: bootstrap: %w", err)
	}
	core.Boot = boot
	core.Logger = newLogger(boot.LogLevel)
	core.Logger.Info("starting hugen", "info", boot.Info())

	if err := installBundledSkills(boot.StateDir, core.Logger); err != nil {
		return nil, fmt.Errorf("buildRuntimeCore: install bundled skills: %w", err)
	}

	httpSrv, mux, err := startHTTPServer(ctx, boot, core.Logger)
	if err != nil {
		return nil, fmt.Errorf("buildRuntimeCore: auth http: %w", err)
	}
	core.HTTPSrv = httpSrv
	core.Mux = mux

	authSvc, err := buildAuthService(ctx, boot, mux, core.Logger)
	if err != nil {
		return nil, failed("auth", err)
	}
	core.Auth = authSvc

	if boot.Hugr.URL != "" && boot.IsRemoteMode() {
		core.RemoteQuerier = connectRemote(boot, authSvc, core.Logger)
	}
	core.Identity = buildIdentity(boot, core.RemoteQuerier)

	cfgSvc, err := buildConfigService(ctx, boot, core.Identity)
	if err != nil {
		return nil, failed("config", err)
	}
	core.Config = cfgSvc

	if err := authSvc.LoadFromView(ctx, cfgSvc.Auth()); err != nil {
		return nil, failed("auth_sources", err)
	}

	localView := core.Config.Local()
	embedView := core.Config.Embedding()
	modelsView := core.Config.Models()

	if localView.LocalDBEnabled() {
		eng, err := buildLocalEngine(ctx, localView, embedView, core.Identity, core.Logger)
		if err != nil {
			return nil, failed("local_engine", err)
		}
		core.LocalEngine = eng
		core.LocalQuerier = eng
	}

	embed := embedView.EmbeddingConfig()
	embedderEnabled := embed.Mode != "" && embed.Model != ""
	store := chooseStore(core.LocalQuerier, core.RemoteQuerier, embedderEnabled)
	if store == nil {
		return nil, failed("store", fmt.Errorf("no querier available (need local engine or remote hub)"))
	}
	core.Store = store

	modelService := models.New(ctx, core.LocalQuerier, core.RemoteQuerier, modelsView, models.WithLogger(core.Logger))
	modelMap := models.BuildModelMap(modelService)
	modelDefaults := models.IntentDefaults(modelService)
	router, err := model.NewModelRouter(modelDefaults, modelMap)
	if err != nil {
		return nil, failed("models", err)
	}
	core.Models = router
	core.Logger.Info("model router ready",
		"default", modelDefaults[model.IntentDefault].String(),
		"cheap", modelDefaults[model.IntentCheap].String())

	agentInfo, err := core.Identity.Agent(ctx)
	if err != nil {
		return nil, failed("identity", err)
	}
	constitution, err := loadConstitution(boot.StateDir, core.Logger)
	if err != nil {
		return nil, failed("constitution", err)
	}
	agent, err := session.NewAgent(agentInfo.ID, agentInfo.Name, core.Identity, constitution)
	if err != nil {
		return nil, failed("agent", err)
	}
	core.Agent = agent

	cmds := session.NewCommandRegistry()
	if err := registerBuiltinCommands(cmds, core.Logger); err != nil {
		return nil, failed("commands", err)
	}
	core.Commands = cmds

	core.Codec = protocol.NewCodec()

	skills, skillStore, err := buildSkillStack(core)
	if err != nil {
		return nil, failed("skills", err)
	}
	core.Skills = skills
	core.SkillStore = skillStore

	core.Permissions = buildPermissionService(core)

	tools, err := buildToolStack(core, core.Permissions, skills)
	if err != nil {
		return nil, failed("tools", err)
	}
	core.Tools = tools

	if err := cmds.Register("skill", session.CommandSpec{
		Handler:     skillCommandHandler(core.Skills, core.SkillStore, core.Permissions),
		Description: "list, load or unload skills: /skill list | /skill load <name> | /skill unload <name>",
	}); err != nil {
		return nil, failed("commands_skill", err)
	}

	core.workspaces = newSessionWorkspaces()
	core.Manager = session.NewManager(
		core.Store, agent, router, cmds, core.Codec, core.Logger,
		session.WithLifecycle(buildSessionLifecycle(core, core.workspaces)),
		session.WithSessionOptions(
			session.WithTools(core.Tools),
			session.WithSkills(core.Skills),
		),
	)

	// /api/auth/agent-token is mounted inside buildToolStack when
	// the deployment carries a `hugr` auth source — see
	// hugr_query.go:buildAgentTokenStore. No-Hugr deployments
	// leave the path unmounted (404), which US5 expects.

	return core, nil
}

// cleanupPartial closes every resource the build had opened so far.
// Called from the partial-failure paths in buildRuntimeCore; mirrors
// the cleanup Shutdown does on the success path. Idempotent — uses
// the same per-resource nil checks Shutdown uses.
func (c *RuntimeCore) cleanupPartial() {
	if c.HTTPSrv != nil {
		shutdownHTTPServer(c.HTTPSrv, c.Logger)
	}
	if c.LocalEngine != nil {
		if err := c.LocalEngine.Close(); err != nil {
			c.Logger.Warn("cleanup: close local engine", "err", err)
		}
	}
	if c.RemoteQuerier != nil {
		c.RemoteQuerier.CloseSubscriptions()
	}
}

// Shutdown closes every resource RuntimeCore owns. Safe to call
// multiple times; subsequent calls are no-ops.
//
// Order, per contracts/runtime-core.md §"Shutdown ordering rationale":
//
//  1. Stop accepting new HTTP traffic. http.Server.Shutdown blocks
//     until in-flight handlers (including SSE writers in slice B)
//     complete or the deadline passes.
//  2. Suspend live sessions — persists each session's status before
//     the embedded engine closes underneath it.
//  3. Close the local engine (DuckDB file handles + WAL flush).
//  4. Drain in-flight remote subscriptions on the upstream client.
func (c *RuntimeCore) Shutdown(ctx context.Context) {
	c.shutdownOnce.Do(func() {
		if c.HTTPSrv != nil {
			c.Logger.Info("shutdown: stop accepting new HTTP connections")
			shutdownHTTPServer(c.HTTPSrv, c.Logger)
		}
		if c.Manager != nil {
			c.Logger.Info("shutdown: suspending sessions")
			sCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			c.Manager.ShutdownAll(sCtx)
			cancel()
			c.Logger.Info("shutdown: sessions persisted")
		}
		if c.Tools != nil {
			if err := c.Tools.Close(); err != nil {
				c.Logger.Warn("shutdown: close tool manager", "err", err)
			}
		}
		if c.LocalEngine != nil {
			if err := c.LocalEngine.Close(); err != nil {
				c.Logger.Warn("shutdown: close local engine", "err", err)
			}
		}
		if c.RemoteQuerier != nil {
			c.RemoteQuerier.CloseSubscriptions()
		}
		c.Logger.Info("shutdown: complete")
	})
}
