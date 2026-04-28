package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// startHTTPServer binds the auth/callback listener and starts a
// goroutine to serve it. The drain goroutine is intentionally absent
// — the caller is responsible for invoking [shutdownHTTPServer] on
// every exit path (success and failure alike). This keeps a failed
// boot from leaking the listener.
func startHTTPServer(_ context.Context, boot *BootstrapConfig, logger *slog.Logger) (*http.Server, *http.ServeMux, error) {
	mux := http.NewServeMux()
	httpSrv := &http.Server{
		Addr:    fmt.Sprintf(":%d", boot.Port),
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

// shutdownHTTPServer drains the auth/callback listener with a short
// timeout. Safe to call multiple times; safe on a nil server.
func shutdownHTTPServer(srv *http.Server, logger *slog.Logger) {
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
