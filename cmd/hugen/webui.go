package main

import (
	"context"
	"fmt"
	"time"

	httpadapter "github.com/hugr-lab/hugen/pkg/adapter/http"
	"github.com/hugr-lab/hugen/pkg/adapter/webui"
	"github.com/hugr-lab/hugen/pkg/runtime"
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
	devToken := httpadapter.NewDevTokenStore()
	core.Logger.Info("dev token issued (loopback only)", "endpoint", "/api/auth/dev-token")

	replay, ok := core.Store.(httpadapter.ReplaySource)
	if !ok {
		core.Logger.Error("runtime store does not implement ReplaySource")
		return 1
	}

	httpAd, err := httpadapter.NewAdapter(httpadapter.Options{
		Mux:      core.Mux,
		Auth:     devToken,
		Codec:    core.Codec,
		Replay:   replay,
		Logger:   core.Logger.With("adapter", "http"),
		DevToken: devToken,
	})
	if err != nil {
		core.Logger.Error("build http adapter", "err", err)
		return 1
	}

	webuiAd := webui.NewAdapter("127.0.0.1", core.Boot.WebUIPort,
		core.Logger.With("adapter", "webui"))

	rt := runtime.NewRuntime(core.Manager,
		[]runtime.Adapter{httpAd, webuiAd}, core.Logger)
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
