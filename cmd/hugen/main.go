// Package main is the entry point for the hugen runtime.
//
// Phase-2 startup flow:
//
//  1. buildRuntimeCore brings up auth, identity, model router,
//     session manager, codec, command registry, and the auth HTTP
//     server. This is done exactly once per process and is owned by
//     main; subcommand handlers never re-bootstrap.
//  2. Dispatch on os.Args[1]:
//     console — attaches the console adapter (phase 1 default).
//     webui  — attaches http + webui adapters (phase 2; pending US1+US2).
//     a2a    — refused (returns in phase 10).
//  3. Block on ctx until SIGINT/SIGTERM, then defer-shutdown core.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hugr-lab/query-engine/types"

	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/hugr-lab/hugen/pkg/identity"
	"github.com/hugr-lab/hugen/pkg/runtime"
)

const (
	exitOK    = 0
	exitUsage = 64
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

// run is the testable entry point. It returns the process exit code
// and writes refusal/usage text to errOut.
func run(args []string, errOut io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "a2a":
		fmt.Fprintln(errOut, "the a2a mode is not yet available in this build; planned for phase 10")
		return exitUsage
	case "webui":
		fmt.Fprintln(errOut, "the webui mode is not yet available in this build; pending US1+US2 of phase 2")
		return exitUsage
	case "", "console":
		// OK — fall through to bootstrap.
	default:
		fmt.Fprintf(errOut, "unknown subcommand %q\n\n", sub)
		fmt.Fprintln(errOut, "usage: hugen [console]")
		fmt.Fprintln(errOut, "  console  start the console adapter (the only mode in phase 1; webui pending US1+US2)")
		return exitUsage
	}

	core, err := buildRuntimeCore(ctx)
	if err != nil {
		fmt.Fprintf(errOut, "%v\n", err)
		return 1
	}
	defer func() {
		sCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		core.Shutdown(sCtx)
	}()

	return runConsole(ctx, core)
}

// chooseStore prefers the local querier when the embedded engine is
// available; falls back to remote.
func chooseStore(localQ, remoteQ types.Querier, embedderEnabled bool) runtime.RuntimeStore {
	if localQ != nil {
		return runtime.NewRuntimeStoreLocal(localQ, embedderEnabled)
	}
	if remoteQ != nil {
		return runtime.NewRuntimeStoreLocal(remoteQ, embedderEnabled)
	}
	return nil
}

func newLogger(level string) *slog.Logger {
	lv := slog.LevelInfo
	if level == "debug" {
		lv = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv}))
}

// silence unused import warnings if main grows lean.
var _ identity.Source = (identity.Source)(nil)
var _ *auth.Service
