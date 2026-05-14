package main

import (
	"context"
	"time"

	"github.com/hugr-lab/hugen/pkg/adapter/tui"
	"github.com/hugr-lab/hugen/pkg/runtime"
	"github.com/hugr-lab/hugen/pkg/session/manager"
)

// runTUI attaches the Bubble Tea TUI adapter to a shared
// *runtime.Core and runs the runtime until ctx cancels. Phase 5.1c
// slice 1: single root session; multi-root tabs land in slice 4.
func runTUI(ctx context.Context, core *runtime.Core) int {
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
