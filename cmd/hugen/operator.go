package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/manager"
)

// tryFindResumableSession picks the most recent resumable root
// session id, or "" if none exist. Adapter handlers (tui, webui)
// use it to auto-resume a crashed-and-restarted process into the
// last live root, so the operator doesn't manually re-attach.
func tryFindResumableSession(ctx context.Context, m *manager.Manager, logger *slog.Logger) string {
	rows, err := m.ListResumableRoots(ctx)
	if err != nil {
		logger.Warn("list resumable roots", "err", err)
		return ""
	}
	if len(rows) == 0 {
		return ""
	}
	logger.Info("resumable sessions found", "count", len(rows), "resuming", rows[0].ID)
	return rows[0].ID
}

// operatorParticipant builds the user-side ParticipantInfo the
// runtime stamps on inbound frames. Reads $USER, falls back to
// "operator" when unset (CI / container).
func operatorParticipant() protocol.ParticipantInfo {
	id := os.Getenv("USER")
	if id == "" {
		id = "operator"
	}
	return protocol.ParticipantInfo{ID: id, Kind: protocol.ParticipantUser, Name: id}
}
