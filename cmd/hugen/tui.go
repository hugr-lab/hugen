package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/hugr-lab/hugen/pkg/adapter/tui"
	"github.com/hugr-lab/hugen/pkg/runtime"
	"github.com/hugr-lab/hugen/pkg/session/manager"
)

// runTUI attaches the Bubble Tea TUI adapter to a shared
// *runtime.Core and runs the runtime until ctx cancels. Phase 5.1c
// slice 1: single root session; multi-root tabs land in slice 4.
func runTUI(ctx context.Context, core *runtime.Core) int {
	// Redirect stderr to a per-run log file so the runtime's slog
	// output (and any third-party stderr writers) does not interleave
	// with bubbletea's altscreen rendering. dup2 swaps fd 2 to the
	// log file in-place; loggers that already captured *os.File for
	// os.Stderr keep working — fd 2 just now writes to the file.
	logPath, restore := redirectStderrToFile(".hugen/tui.log")
	defer restore()
	if logPath != "" {
		core.Logger.Info("tui: stderr → log file", "path", logPath)
	}

	resumeID := tryFindResumableSession(ctx, core.Manager, core.Logger)

	tuiAdapter := tui.New(
		tui.WithLogger(core.Logger),
		tuiResumeOption(resumeID),
		tui.WithUser(operatorParticipant()),
	)

	rt := manager.NewRuntime(core.Manager, []manager.Adapter{tuiAdapter}, core.Logger)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = rt.Shutdown(shutdownCtx)
	}()

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

func tuiResumeOption(id string) tui.Option {
	if id == "" {
		return func(*tui.Adapter) {}
	}
	return tui.WithResumeSession(id)
}

// redirectStderrToFile opens (or creates) the given log path and
// duplicates it onto fd 2 (os.Stderr). Returns the absolute log
// path and a restore function the caller MUST defer to put fd 2
// back to the original terminal — otherwise an unexpected panic
// after the program exits would write to the closed log file.
//
// On any error opening the file the function returns ("", noop) and
// stderr stays pointed at the terminal. The TUI still launches but
// the operator sees logs leak into altscreen — same as before the
// fix; better than refusing to start.
func redirectStderrToFile(relPath string) (string, func()) {
	noop := func() {}
	// Phase 5.1c S1 — owner-only access. stderr can carry logs with
	// secrets (panic dumps, third-party tool errors mentioning
	// tokens) so the file MUST NOT be world-readable.
	if err := os.MkdirAll(filepath.Dir(relPath), 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "tui: cannot create log dir: %v\n", err)
		return "", noop
	}
	f, err := os.OpenFile(relPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tui: cannot open log file: %v\n", err)
		return "", noop
	}
	// Save the original stderr fd so the deferred restore can put it
	// back; dup the current fd 2 to a fresh number we'll close later.
	origFd, dupErr := syscall.Dup(int(os.Stderr.Fd()))
	if dupErr != nil {
		_ = f.Close()
		fmt.Fprintf(os.Stderr, "tui: cannot dup stderr: %v\n", dupErr)
		return "", noop
	}
	if err := syscall.Dup2(int(f.Fd()), int(os.Stderr.Fd())); err != nil {
		_ = syscall.Close(origFd)
		_ = f.Close()
		fmt.Fprintf(os.Stderr, "tui: cannot redirect stderr: %v\n", err)
		return "", noop
	}
	abs, _ := filepath.Abs(relPath)
	restore := func() {
		// Best-effort: put fd 2 back to the terminal so post-TUI
		// output (panic dumps, fmt.Fprintln to os.Stderr in caller
		// defers) reaches the user.
		_ = syscall.Dup2(origFd, int(os.Stderr.Fd()))
		_ = syscall.Close(origFd)
		_ = f.Close()
	}
	return abs, restore
}
