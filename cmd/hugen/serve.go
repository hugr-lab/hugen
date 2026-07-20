package main

import (
	"context"
	"fmt"
	"time"

	"github.com/hugr-lab/hugen/pkg/adapter/httpapi"
	artifactext "github.com/hugr-lab/hugen/pkg/extension/artifact"
	schedext "github.com/hugr-lab/hugen/pkg/extension/scheduler"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/runtime"
	"github.com/hugr-lab/hugen/pkg/session/manager"
)

// runServe attaches the native HTTP API adapter to a shared *runtime.Core and
// runs the runtime until ctx cancels. The `hugen serve` run mode — the ONE
// interaction surface for hub-mode hugen, driven by an external gateway / UI.
// H1 of design/008-integration/spec-http-api.md.
//
// Listener mode is env-driven (HUGEN_API_PORT): 0 → mount on the runtime's
// existing auth/callback listener (core.Mux); >0 → a dedicated listener (the
// norm). Forwarded user tokens are verified against the hub issuer (HUGR_ISSUER,
// reused); with none configured the endpoint fails closed unless
// HUGEN_API_ALLOW_OPEN=1. Transport config is env, never the agent YAML.
func runServe(ctx context.Context, core *runtime.Core, boot *BootstrapConfig) int {
	opts := []httpapi.Option{
		httpapi.WithLogger(core.Logger),
		httpapi.WithBaseURL(apiBaseURL(boot)),
		httpapi.WithIssuer(boot.Hugr.Issuer),
		httpapi.WithAllowOpen(boot.APIAllowOpen),
		httpapi.WithDevUI(boot.APIDevUI),
	}
	// H2: verify forwarded user tokens against hugr's authority (auth.me).
	// Allow-open (dev) skips it — every request becomes the local dev user.
	if !boot.APIAllowOpen {
		opts = append(opts, httpapi.WithVerifier(userTokenVerifier(core)))
	}
	// H6: artifact endpoints, backed by the artifact extension.
	if core.Artifacts != nil {
		opts = append(opts, httpapi.WithArtifactStore(artifactShim{core.Artifacts}))
	}
	// Per-session scheduled-task list, backed by the scheduler store.
	if core.TaskStore != nil {
		opts = append(opts, httpapi.WithTaskStore(core.TaskStore))
	}
	// Task-lifecycle writes (cancel/delete) go through the scheduler extension
	// so store + in-memory runner index stay in sync.
	for _, ext := range core.Extensions {
		if sched, ok := ext.(*schedext.Extension); ok {
			opts = append(opts, httpapi.WithTaskController(sched))
			break
		}
	}
	// SK6: manual skills-reconcile poke, backed by the marketplace reconciler.
	if core.HasMarketplace() {
		opts = append(opts, httpapi.WithSkillsRefresher(func(ctx context.Context) (any, error) {
			return core.RefreshSkills(ctx)
		}))
	}
	if boot.APIPort > 0 {
		opts = append(opts, httpapi.WithListenPort(boot.APIPort))
		core.Logger.Info("httpapi: dedicated listener mode", "port", boot.APIPort)
	} else {
		opts = append(opts, httpapi.WithSharedMux(core.Mux))
		core.Logger.Info("httpapi: shared auth-listener mode", "port", boot.Port)
	}

	rt := manager.NewRuntime(core.Manager, []manager.Adapter{httpapi.New(opts...)}, core.Logger)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = rt.Shutdown(shutdownCtx)
	}()

	// SK2: start the background skills reconciler (no-op without a hub
	// marketplace configured). Async first pass — never blocks serving.
	core.StartSkillReconciler(ctx)

	if err := rt.Start(ctx); err != nil {
		if ctx.Err() != nil {
			core.Logger.Info("shutdown complete")
			return exitOK
		}
		core.Logger.Error("runtime exited", "err", err)
		return 1
	}
	return exitOK
}

// artifactShim adapts core.Artifacts (Store.List/Path + Extension.Ingest) to
// httpapi.ArtifactStore.
type artifactShim struct{ ext *artifactext.Extension }

func (s artifactShim) List(rootID string) ([]protocol.ArtifactRef, error) {
	return s.ext.Store().List(rootID)
}
func (s artifactShim) Path(rootID, id string) (string, error) { return s.ext.Store().Path(rootID, id) }
func (s artifactShim) Ingest(rootID, src, name string) (protocol.ArtifactRef, error) {
	return s.ext.Ingest(rootID, src, name)
}

// apiBaseURL resolves the public URL the agent card advertises. Explicit
// HUGEN_API_BASE_URL wins; otherwise derived — dedicated → localhost:<port>,
// shared → the runtime base URL.
func apiBaseURL(boot *BootstrapConfig) string {
	if boot.APIBaseURL != "" {
		return boot.APIBaseURL
	}
	if boot.APIPort > 0 {
		return fmt.Sprintf("http://localhost:%d", boot.APIPort)
	}
	return boot.BaseURI
}
