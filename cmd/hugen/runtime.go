package main

import (
	"context"
	"fmt"
	"log/slog"
	stdhttp "net/http"
	"sync"
	"time"

	"github.com/hugr-lab/query-engine/types"

	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/hugr-lab/hugen/pkg/identity"
	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/models"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/runtime"
)

// RuntimeCore aggregates every dependency a subcommand handler needs.
// Constructed once by buildRuntimeCore at process start; fields are
// read-only afterwards (constitution principle II — no Set* mutators).
//
// See specs/002-agent-runtime-phase-2/contracts/runtime-core.md for
// the full contract.
type RuntimeCore struct {
	Boot     *BootstrapConfig
	Cfg      *RuntimeConfig
	Logger   *slog.Logger
	Auth     *auth.Service
	Identity identity.Source
	Agent    *runtime.Agent
	Models   *model.ModelRouter
	Manager  *runtime.SessionManager
	Commands *runtime.CommandRegistry
	Codec    *protocol.Codec

	// HTTPSrv hosts the auth endpoints (phase 1) and, in phase 2,
	// /api/v1/* via pkg/adapter/http. Both share the same mux so the
	// existing bearer middleware applies uniformly.
	HTTPSrv *stdhttp.Server
	Mux     *stdhttp.ServeMux

	// Stored so cmd-level helpers and adapters can reuse them.
	LocalQuerier  types.Querier
	RemoteQuerier types.Querier

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

	boot, err := loadBootstrapConfig(".env")
	if err != nil {
		return nil, fmt.Errorf("buildRuntimeCore: bootstrap: %w", err)
	}
	core.Boot = boot
	core.Logger = newLogger(boot.LogLevel)
	core.Logger.Info("starting hugen", "info", boot.Info())

	httpSrv, mux, err := startHTTPServer(ctx, boot, core.Logger)
	if err != nil {
		return nil, fmt.Errorf("buildRuntimeCore: auth http: %w", err)
	}
	core.HTTPSrv = httpSrv
	core.Mux = mux

	authSvc, err := buildAuthService(ctx, boot, mux, core.Logger)
	if err != nil {
		shutdownHTTPServer(httpSrv, core.Logger)
		return nil, fmt.Errorf("buildRuntimeCore: auth: %w", err)
	}
	core.Auth = authSvc

	if boot.Hugr.URL != "" && boot.IsRemoteMode() {
		core.RemoteQuerier = connectRemote(boot, authSvc, core.Logger)
	}
	core.Identity = buildIdentity(boot, core.RemoteQuerier)

	cfg, err := buildRuntimeConfig(ctx, boot, core.Identity)
	if err != nil {
		shutdownHTTPServer(httpSrv, core.Logger)
		return nil, fmt.Errorf("buildRuntimeCore: runtime_config: %w", err)
	}
	core.Cfg = cfg

	if cfg.LocalDBEnabled() {
		core.LocalQuerier, err = buildLocalEngine(ctx, cfg, core.Identity, core.Logger)
		if err != nil {
			shutdownHTTPServer(httpSrv, core.Logger)
			return nil, fmt.Errorf("buildRuntimeCore: local_engine: %w", err)
		}
	}

	modelService := models.New(ctx, core.LocalQuerier, core.RemoteQuerier, cfg.Models, models.WithLogger(core.Logger))
	modelMap := models.BuildModelMap(modelService)
	modelDefaults := models.IntentDefaults(modelService)
	router, err := model.NewModelRouter(modelDefaults, modelMap)
	if err != nil {
		shutdownHTTPServer(httpSrv, core.Logger)
		return nil, fmt.Errorf("buildRuntimeCore: models: %w", err)
	}
	core.Models = router
	core.Logger.Info("model router ready",
		"default", modelDefaults[model.IntentDefault].String(),
		"cheap", modelDefaults[model.IntentCheap].String())

	embedderEnabled := cfg.Embedding.Mode != "" && cfg.Embedding.Model != ""
	store := chooseStore(core.LocalQuerier, core.RemoteQuerier, embedderEnabled)
	if store == nil {
		shutdownHTTPServer(httpSrv, core.Logger)
		return nil, fmt.Errorf("buildRuntimeCore: store: no querier available (need local engine or remote hub)")
	}

	agentInfo, err := core.Identity.Agent(ctx)
	if err != nil {
		shutdownHTTPServer(httpSrv, core.Logger)
		return nil, fmt.Errorf("buildRuntimeCore: identity: %w", err)
	}
	agent, err := runtime.NewAgent(agentInfo.ID, agentInfo.Name, core.Identity)
	if err != nil {
		shutdownHTTPServer(httpSrv, core.Logger)
		return nil, fmt.Errorf("buildRuntimeCore: agent: %w", err)
	}
	core.Agent = agent

	cmds := runtime.NewCommandRegistry()
	if err := registerBuiltinCommands(cmds, core.Logger); err != nil {
		shutdownHTTPServer(httpSrv, core.Logger)
		return nil, fmt.Errorf("buildRuntimeCore: commands: %w", err)
	}
	core.Commands = cmds

	core.Codec = protocol.NewCodec()
	core.Manager = runtime.NewSessionManager(store, agent, router, cmds, core.Codec, core.Logger)

	return core, nil
}

// Shutdown closes every resource RuntimeCore owns. Safe to call
// multiple times; subsequent calls are no-ops.
//
// Order: stop accepting new HTTP traffic → suspend live sessions.
// Per contracts/runtime-core.md §"Shutdown ordering rationale": new
// requests can spawn new sessions, so listener-stop precedes
// session-suspend.
func (c *RuntimeCore) Shutdown(ctx context.Context) {
	c.shutdownOnce.Do(func() {
		if c.HTTPSrv != nil {
			c.Logger.Info("shutdown: stop accepting new HTTP connections")
			shutdownHTTPServer(c.HTTPSrv, c.Logger)
		}
		if c.Manager != nil {
			c.Logger.Info("shutdown: suspending sessions")
			sCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			c.Manager.ShutdownAll(sCtx)
			c.Logger.Info("shutdown: sessions persisted")
		}
		c.Logger.Info("shutdown: complete")
	})
}
