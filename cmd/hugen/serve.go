package main

import (
	"context"
	"fmt"
	"time"

	"github.com/hugr-lab/hugen/pkg/adapter/httpapi"
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
