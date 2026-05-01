// Package main is the entry point for the hugen session.
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
	"github.com/hugr-lab/hugen/pkg/session"
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
	case "", "console", "webui":
		// OK — fall through to bootstrap.
	default:
		fmt.Fprintf(errOut, "unknown subcommand %q\n\n", sub)
		fmt.Fprintln(errOut, "usage: hugen [console|webui]")
		fmt.Fprintln(errOut, "  console  start the console adapter")
		fmt.Fprintln(errOut, "  webui    start the HTTP API + loopback web UI")
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

	switch sub {
	case "webui":
		return runWebUI(ctx, core)
	default:
		return runConsole(ctx, core)
	}
}

// chooseStore picks the querier the runtime store talks to. The
// agent runs in exactly one of two modes:
//
//   - local mode (BootstrapConfig.IsLocalMode + LocalDBEnabled):
//     localQ is the embedded DuckDB. All sessions, events, notes,
//     and memory live inside the agent process.
//   - remote mode: remoteQ is the upstream hugr GraphQL endpoint;
//     localQ is nil. Sessions/memory/artifacts persist in the
//     shared hub DB and the agent identifies itself by the bearer
//     token its identity source supplies. The schema is the same —
//     session.NewRuntimeStoreLocal is mode-agnostic; the "local"
//     in its name refers to the Go-side facade, not the DB.
//
// Mixing the two queriers would split state across stores and is
// not supported.
func chooseStore(localQ, remoteQ types.Querier, embedderEnabled bool) session.RuntimeStore {
	if localQ != nil {
		return session.NewRuntimeStoreLocal(localQ, embedderEnabled)
	}
	if remoteQ != nil {
		return session.NewRuntimeStoreLocal(remoteQ, embedderEnabled)
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
