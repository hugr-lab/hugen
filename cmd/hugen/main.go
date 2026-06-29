// Package main is the entry point for the hugen session.
//
// Startup flow:
//
//  1. bootRuntime brings up auth, identity, model router, session
//     manager, codec, command registry, and the auth HTTP server via
//     pkg/runtime.Build. This runs exactly once per process and is
//     owned by main; subcommand handlers never re-bootstrap.
//  2. Dispatch on os.Args[1]:
//     tui    — attaches the Bubble Tea TUI adapter (default).
//     a2a    — attaches the A2A protocol adapter (design/008, Stage 1).
//  3. Block on ctx until SIGINT/SIGTERM, then defer-shutdown core.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
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
	// Expose hugen's startup cwd to config.yaml as ${HUGEN_VENDOR_DIR}
	// so per_session MCP args that reference vendored sources (e.g.
	// `uvx --from ${HUGEN_VENDOR_DIR}/mcp-server-motherduck`) resolve
	// correctly. Without this, uvx interprets a relative path against
	// its own cwd (= session workspace) and fails. Operators who want
	// a different vendor location can set HUGEN_VENDOR_DIR explicitly
	// before launching hugen.
	if cwd, err := os.Getwd(); err == nil {
		if _, ok := os.LookupEnv("HUGEN_VENDOR_DIR"); !ok {
			_ = os.Setenv("HUGEN_VENDOR_DIR", filepath.Join(cwd, "vendor"))
		}
	}

	switch sub {
	case "a2a", "", "tui":
		// OK — fall through to bootstrap.
	default:
		fmt.Fprintf(errOut, "unknown subcommand %q\n\n", sub)
		fmt.Fprintln(errOut, "usage: hugen [tui|a2a]")
		fmt.Fprintln(errOut, "  tui    start the Bubble Tea TUI adapter (default)")
		fmt.Fprintln(errOut, "  a2a    start the A2A protocol adapter (headless; Teams/Copilot interop)")
		return exitUsage
	}

	core, boot, err := bootRuntime(ctx)
	if err != nil {
		fmt.Fprintf(errOut, "%v\n", err)
		return 1
	}
	defer func() {
		sCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		core.Shutdown(sCtx)
	}()

	if sub == "a2a" {
		return runA2A(ctx, core, boot)
	}
	return runTUI(ctx, core)
}

func newLogger(level string) *slog.Logger {
	lv := slog.LevelInfo
	if level == "debug" {
		lv = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv}))
}
