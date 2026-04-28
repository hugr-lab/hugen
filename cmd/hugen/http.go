package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

func startHTTPServer(ctx context.Context, boot *BootstrapConfig, logger *slog.Logger) (*http.Server, *http.ServeMux, error) {
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
	go func() {
		<-ctx.Done()
		logger.Info("shutting down HTTP server: draining requests")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			logger.Error("HTTP server shutdown", "err", err)
		}
	}()
	return httpSrv, mux, nil
}
