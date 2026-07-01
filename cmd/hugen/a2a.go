package main

import (
	"context"
	"fmt"
	"time"

	"github.com/hugr-lab/hugen/pkg/adapter/a2a"
	"github.com/hugr-lab/hugen/pkg/runtime"
	"github.com/hugr-lab/hugen/pkg/session/manager"
)

// runA2A attaches the A2A protocol adapter to a shared *runtime.Core and
// runs the runtime until ctx cancels. Headless sibling of runTUI — the
// `hugen a2a` run mode. Stage 1 of design/008-integration.
//
// Listener mode is env-driven (HUGEN_A2A_PORT): 0 → mount on the runtime's
// existing auth/callback listener (core.Mux); >0 → a dedicated listener.
// Transport config is env, never the agent YAML (config.yaml is the agent).
func runA2A(ctx context.Context, core *runtime.Core, boot *BootstrapConfig) int {
	opts := []a2a.Option{
		a2a.WithLogger(core.Logger),
		a2a.WithBaseURL(a2aBaseURL(boot)),
		a2a.WithAPIKey(boot.A2AAPIKey),
		a2a.WithAllowOpen(boot.A2AAllowOpen),
	}
	// A10: by-ref artifact delivery — published files surface as FileParts
	// pointing at the adapter's signed download endpoint, resolved through the
	// artifact store. Only when artifacts are enabled.
	if core.Artifacts != nil {
		opts = append(opts, a2a.WithArtifactResolver(core.Artifacts.Store().Path))
	}
	if boot.A2APort > 0 {
		opts = append(opts, a2a.WithListenPort(boot.A2APort))
		core.Logger.Info("a2a: dedicated listener mode", "port", boot.A2APort)
	} else {
		opts = append(opts, a2a.WithSharedMux(core.Mux))
		core.Logger.Info("a2a: shared auth-listener mode", "port", boot.Port)
	}

	rt := manager.NewRuntime(core.Manager, []manager.Adapter{a2a.New(opts...)}, core.Logger)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = rt.Shutdown(shutdownCtx)
	}()

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

// a2aBaseURL resolves the public URL the agent card advertises. Explicit
// HUGEN_A2A_BASE_URL wins (the tunnel hostname in production); otherwise it
// is derived — dedicated listener → localhost:<port>, shared → the runtime
// base URL.
func a2aBaseURL(boot *BootstrapConfig) string {
	if boot.A2ABaseURL != "" {
		return boot.A2ABaseURL
	}
	if boot.A2APort > 0 {
		return fmt.Sprintf("http://localhost:%d", boot.A2APort)
	}
	return boot.BaseURI
}
