// Package main is the entry point for the hugen runtime.
//
// Startup flow:
//
//  1. Load bootstrap config (.env → HUGR_URL, HUGR_ACCESS_TOKEN, …).
//  2. Bring up the runtime once, independent of mode: auth
//     SourceRegistry on a shared mux, hugr client, full config
//     (local YAML or remote hub pull), ADK LLM agent backed by a
//     hugr LLM model, in-memory session service.
//  3. Dispatch to one of three mode handlers based on os.Args[1]:
//     a2a (default), console, webui. Each handler only wires its
//     mode-specific HTTP layout on top of the prepared runtime.
//  4. Block on ctx until SIGINT/SIGTERM, then shut down cleanly.
package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/hugr-lab/hugen/pkg/config"
)

// app bundles everything bootstrap produces. Mode-specific serve*
// helpers read from it; they never re-touch the startup phases.
type app struct {
	boot    *config.BootstrapConfig
	cfg     *config.Config
	logger  *slog.Logger
	runtime *runtime
	authReg *auth.SourceRegistry

	// authMux has /auth/callback + per-Source /auth/login/<name>
	// mounted by auth.SourceRegistry.Mount. Mode handlers add their
	// own routes (agent card, invoke) on the same mux.
	authMux *http.ServeMux

	// prompts are OIDC prompt-login hooks to fire once the HTTP
	// listener for authMux is bound.
	prompts []func()
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	boot, err := config.LoadBootstrap(".env")
	if err != nil {
		log.Fatalf("bootstrap: %v", err)
	}

	logger := newLogger()
	logger.Info("hugen starting",
		"hugr_url", boot.Hugr.URL,
		"mode", modeLabel(boot),
	)

	a, err := bootstrap(ctx, boot, logger)
	if err != nil {
		log.Fatalf("bootstrap: %v", err)
	}
	defer a.runtime.close(logger)

	sub := ""
	if len(os.Args) > 1 {
		sub = os.Args[1]
	}

	switch sub {
	case "console":
		err = serveConsole(ctx, a)
	case "webui":
		err = serveWebUI(ctx, a)
	default:
		err = serveA2A(ctx, a)
	}
	if err != nil && ctx.Err() == nil {
		log.Fatalf("%s: %v", sub, err)
	}

	logger.Info("shutdown complete")
}

// serve binds a listener for each server, fires prompt-login hooks
// once every listener is live, and blocks until ctx is cancelled or
// one of the servers errors. On ctx cancel it triggers graceful
// shutdown on every server concurrently.
//
// Returns the first non-trivial serve error; http.ErrServerClosed is
// treated as a clean exit.
func serve(ctx context.Context, servers []*http.Server, prompts []func(), logger *slog.Logger) error {
	if len(servers) == 0 {
		return fmt.Errorf("serve: no servers")
	}

	type result struct {
		err  error
		addr string
	}
	results := make(chan result, len(servers))
	listeners := make([]net.Listener, 0, len(servers))

	for _, srv := range servers {
		ln, err := net.Listen("tcp", srv.Addr)
		if err != nil {
			for _, l := range listeners {
				_ = l.Close()
			}
			return fmt.Errorf("listen %s: %w", srv.Addr, err)
		}
		listeners = append(listeners, ln)
		s := srv
		go func() {
			err := s.Serve(ln)
			results <- result{err: err, addr: s.Addr}
		}()
	}
	for _, p := range prompts {
		go p()
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		logger.Info("shutting down: draining requests")
		for _, s := range servers {
			_ = s.Shutdown(shutdownCtx)
		}
	}()

	var firstErr error
	for range servers {
		r := <-results
		if r.err != nil && r.err != http.ErrServerClosed && firstErr == nil {
			firstErr = fmt.Errorf("serve %s: %w", r.addr, r.err)
		}
	}
	return firstErr
}

func newLogger() *slog.Logger {
	level := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") == "debug" {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

func modeLabel(boot *config.BootstrapConfig) string {
	if boot.Remote() {
		return "remote"
	}
	return "local"
}
