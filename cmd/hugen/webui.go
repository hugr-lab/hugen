package main

import (
	"context"
	"fmt"
	"time"

	httpadapter "github.com/hugr-lab/hugen/pkg/adapter/http"
	"github.com/hugr-lab/hugen/pkg/adapter/webui"
	"github.com/hugr-lab/hugen/pkg/session"
)

// runWebUI attaches the HTTP and web-UI adapters to *RuntimeCore.
// Per contracts/runtime-core.md, the handler MUST NOT re-bootstrap
// any dependency already produced by buildRuntimeCore.
//
// Two adapters share the run-loop:
//
//   - httpadapter.Adapter mounts /api/v1/* on core.Mux (the same mux
//     that hosts /auth/login and /auth/callback).
//   - webui.Adapter binds a separate loopback listener and serves
//     embedded static assets.
func runWebUI(ctx context.Context, core *RuntimeCore) int {
	devToken, err := httpadapter.NewDevTokenStore()
	if err != nil {
		core.Logger.Error("dev-token mint failed", "err", err)
		return 1
	}
	core.Logger.Info("dev token issued (loopback only)", "endpoint", "/api/auth/dev-token")

	replay, ok := core.Store.(httpadapter.ReplaySource)
	if !ok {
		core.Logger.Error("runtime store does not implement ReplaySource")
		return 1
	}

	webuiOriginIP := fmt.Sprintf("http://127.0.0.1:%d", core.Boot.WebUIPort)
	webuiOriginHost := fmt.Sprintf("http://localhost:%d", core.Boot.WebUIPort)
	httpAd, err := httpadapter.NewAdapter(httpadapter.Options{
		Mux:                core.Mux,
		Auth:               devToken,
		Codec:              core.Codec,
		Replay:             replay,
		Logger:             core.Logger.With("adapter", "http"),
		DevToken:           devToken,
		CORSAllowedOrigins: []string{webuiOriginIP, webuiOriginHost},
	})
	if err != nil {
		core.Logger.Error("build http adapter", "err", err)
		return 1
	}
	// buildRuntimeCore has already produced every dep the API
	// needs by this point — flip the gate so /api/v1/* stops
	// returning 503 runtime_starting. (Phase 2 mounts the http
	// adapter after boot completes; the gate exists so a future
	// boot-time mount path doesn't silently expose half-built
	// state.)
	httpAd.MarkReady()

	apiBase := core.Boot.BaseURI
	if apiBase == "" {
		apiBase = fmt.Sprintf("http://127.0.0.1:%d", core.Boot.Port)
	}
	webuiAd := webui.NewAdapter("127.0.0.1", core.Boot.WebUIPort, apiBase,
		core.Logger.With("adapter", "webui"))

	rt := session.NewRuntime(core.Manager,
		[]session.Adapter{httpAd, webuiAd}, core.Logger)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = rt.Shutdown(shutdownCtx)
	}()

	core.Logger.Info("webui ready",
		"api", fmt.Sprintf("http://127.0.0.1:%d/api/v1", core.Boot.Port),
		"ui", fmt.Sprintf("http://127.0.0.1:%d/", core.Boot.WebUIPort))

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
