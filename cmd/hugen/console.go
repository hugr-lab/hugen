package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/hugr-lab/hugen/pkg/adapter/console"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session"
)

// runConsole attaches the console adapter to a shared *RuntimeCore
// and runs the runtime until ctx cancels. Per
// contracts/runtime-core.md, the handler MUST NOT re-bootstrap any
// dependency already produced by buildRuntimeCore.
func runConsole(ctx context.Context, core *RuntimeCore) int {
	resumeID := tryFindResumableSession(ctx, core.Manager, core.Logger)

	consoleAdapter := console.New(
		console.WithLogger(core.Logger),
		consoleResumeOption(resumeID),
		console.WithUser(operatorParticipant()),
	)

	rt := session.NewRuntime(core.Manager, []session.Adapter{consoleAdapter}, core.Logger)
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

func tryFindResumableSession(ctx context.Context, m *session.Manager, logger *slog.Logger) string {
	rows, err := m.ListSessions(ctx, session.StatusActive)
	if err != nil {
		logger.Warn("list active sessions", "err", err)
		return ""
	}
	if len(rows) == 0 {
		return ""
	}
	logger.Info("resumable sessions found", "count", len(rows), "resuming", rows[0].ID)
	return rows[0].ID
}

func consoleResumeOption(id string) console.Option {
	if id == "" {
		return func(*console.Adapter) {}
	}
	return console.WithResumeSession(id)
}

func operatorParticipant() protocol.ParticipantInfo {
	id := os.Getenv("USER")
	if id == "" {
		id = "operator"
	}
	return protocol.ParticipantInfo{ID: id, Kind: protocol.ParticipantUser, Name: id}
}

