package main

import (
	"context"
	"fmt"
	"net/http"

	"github.com/a2aproject/a2a-go/a2asrv"

	"github.com/hugr-lab/hugen/pkg/a2a"
)

// serveA2A is the default mode: agent card + invoke on a single A2A
// listener. OIDC callbacks live on the same mux, so the redirect_uri
// matches what the agent advertises.
func serveA2A(ctx context.Context, a *app) error {
	cardH, invokeH := a2a.BuildHandlers(a.runtime.Agent, a.runtime.Sessions, a.cfg.A2A.BaseURL)
	a.authMux.Handle(a2asrv.WellKnownAgentCardPath, cardH)
	a.authMux.Handle("/invoke", invokeH)

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", a.cfg.A2A.Port),
		Handler: a.authMux,
	}
	a.logger.Info("A2A server listening",
		"addr", srv.Addr,
		"invoke", a.cfg.A2A.BaseURL+"/invoke",
		"card", a.cfg.A2A.BaseURL+a2asrv.WellKnownAgentCardPath,
	)
	return serve(ctx, []*http.Server{srv}, a.prompts, a.logger)
}
