package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/hugr-lab/hugen/pkg/auth/sources/hugr"
)

// StartHTTPServer binds the auth/callback listener and starts a
// goroutine to serve it. The drain goroutine is intentionally absent
// — the caller is responsible for invoking ShutdownHTTPServer on
// every exit path (success and failure alike). This keeps a failed
// boot from leaking the listener.
func StartHTTPServer(cfg HTTPConfig, logger *slog.Logger) (*http.Server, *http.ServeMux, error) {
	mux := http.NewServeMux()
	httpSrv := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Port),
		Handler: mux,
	}
	logger.Info("HTTP server listening", "addr", httpSrv.Addr)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("HTTP server", "err", err)
		}
	}()
	return httpSrv, mux, nil
}

// ShutdownHTTPServer drains the auth/callback listener with a short
// timeout. Safe to call multiple times; safe on a nil server.
func ShutdownHTTPServer(srv *http.Server, logger *slog.Logger) {
	if srv == nil {
		return
	}
	logger.Info("shutting down HTTP server: draining requests")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && err != http.ErrServerClosed {
		logger.Error("HTTP server shutdown", "err", err)
	}
}

// BuildAuthService constructs the *auth.Service that owns the
// authentication mux endpoints. In Mode == "local" with no Hugr URL
// configured, the service still mounts (so the ad-hoc /auth/callback
// works) but no primary source is registered. In remote mode or when
// a Hugr URL is set, a hugr-flavoured source is added as primary.
func BuildAuthService(ctx context.Context, cfg Config, mux *http.ServeMux, logger *slog.Logger) (*auth.Service, error) {
	isRemote := cfg.Mode == "remote"
	as := auth.NewService(logger, mux, cfg.HTTP.BaseURI, cfg.HTTP.Port, isRemote)
	if cfg.Hugr.URL == "" {
		logger.Info("no hugr auth config; skipping hugr auth source")
		return as, nil
	}
	hugrAuth, err := hugr.BuildHugrSource(ctx, hugr.Config{
		BaseURI:     cfg.HTTP.BaseURI,
		RedirectURI: cfg.Hugr.RedirectURI,
		DiscoverURL: cfg.Hugr.URL,
		AccessToken: cfg.Hugr.AccessToken,
		TokenURL:    cfg.Hugr.TokenURL,
		Issuer:      cfg.Hugr.Issuer,
		ClientID:    cfg.Hugr.ClientID,
	}, logger)
	if err != nil {
		return nil, err
	}
	if err := as.AddPrimary(hugrAuth); err != nil {
		return nil, err
	}
	return as, nil
}

// phaseHTTPAuth runs phase 2: starts the HTTP listener and builds
// the auth.Service. Populates Core.HTTPSrv, Core.Mux, Core.Auth.
// Registers a cleanup that drains the HTTP server on Shutdown /
// partial-build failure.
func phaseHTTPAuth(ctx context.Context, core *Core) error {
	srv, mux, err := StartHTTPServer(core.Cfg.HTTP, core.Logger)
	if err != nil {
		return err
	}
	core.HTTPSrv = srv
	core.Mux = mux
	core.addCleanup(func() { ShutdownHTTPServer(srv, core.Logger) })

	authSvc, err := BuildAuthService(ctx, core.Cfg, mux, core.Logger)
	if err != nil {
		return err
	}
	core.Auth = authSvc
	if hook := core.Cfg.AfterAuthHook; hook != nil {
		if err := hook(ctx, authSvc); err != nil {
			return fmt.Errorf("after-auth hook: %w", err)
		}
	}
	return nil
}
