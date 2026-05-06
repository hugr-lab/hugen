// Package main is the entry point for the hugen session.
//
// Startup flow:
//
//  1. bootRuntime brings up auth, identity, model router, session
//     manager, codec, command registry, and the auth HTTP server via
//     pkg/runtime.Build. This runs exactly once per process and is
//     owned by main; subcommand handlers never re-bootstrap.
//  2. Dispatch on os.Args[1]:
//     console — attaches the console adapter (default).
//     webui  — attaches http + webui adapters.
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

	switch sub {
	case "webui":
		return runWebUI(ctx, core, boot)
	default:
		return runConsole(ctx, core)
	}
}

func newLogger(level string) *slog.Logger {
	lv := slog.LevelInfo
	if level == "debug" {
		lv = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv}))
}
