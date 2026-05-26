// Package scheduler hosts the per-session TaskManager extension and
// the Phase 6 system reapers. Phase 6.1b lands the resilient
// `task_log_reap_stuck` reaper alongside the storage layer; the
// fully-fledged TaskManager fire dispatch lives in 6.1c.
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/hugr-lab/hugen/pkg/scheduler/runner"
	"github.com/hugr-lab/hugen/pkg/scheduler/store"
)

// stuckCutoff is the default age a `started` row must exceed before
// the reaper considers it abandoned. Matches the resilience-reaper
// cadence (1h fire) so a fire that crashed mid-flight gets cleaned
// up at the next tick instead of hanging in operator UIs as
// "in flight" forever.
const stuckCutoff = time.Hour

// ReapStuckTaskRuns builds the [runner.RunnerFn] for the
// `task_log_reap_stuck` system runner (Phase 6 spec §16.1).
//
// Reaper contract: every `started` row with no matching terminal
// row whose fire's `planned_at` is older than `stuckCutoff` is
// taken as evidence of a fire that crashed before recording its
// outcome. The reaper closes the loop append-only — INSERT a
// `cancelled` row with `outcome.reason='reap_stuck'` for each
// stuck (task_id, fire_seq). No UPDATEs to the original `started`
// row (append-only invariant).
//
// agentID scopes the reaper to one agent's task surface. logger is
// optional; nil falls back to slog.Default.
func ReapStuckTaskRuns(agentID string, st store.TaskStore, logger *slog.Logger) runner.RunnerFn {
	if logger == nil {
		logger = slog.Default()
	}
	return func(ctx context.Context, fire runner.FireMeta) (runner.Outcome, error) {
		if st == nil {
			return runner.Outcome{}, fmt.Errorf("scheduler/reap: nil TaskStore")
		}
		cutoff := fire.PlannedAt
		if cutoff.IsZero() {
			cutoff = time.Now().UTC()
		}
		cutoff = cutoff.Add(-stuckCutoff)

		stuck, err := st.ListInFlightFires(ctx, agentID, cutoff)
		if err != nil {
			return runner.Outcome{}, fmt.Errorf("scheduler/reap: list in-flight: %w", err)
		}
		if len(stuck) == 0 {
			return runner.Outcome{Summary: "no stuck fires"}, nil
		}

		var reaped, failed int
		for _, s := range stuck {
			err := st.AppendLog(ctx, store.TaskLogEntry{
				TaskID:    s.TaskID,
				AgentID:   s.AgentID,
				FireSeq:   s.FireSeq,
				EventType: store.LogEventCancelled,
				PlannedAt: s.PlannedAt,
				SessionID: s.SessionID,
				Outcome: &store.TaskOutcome{
					Reason:       "reap_stuck",
					ErrorMessage: "fire started but never recorded a terminal event within cutoff",
				},
			})
			if err != nil {
				failed++
				logger.Warn("task_log_reap_stuck: append cancelled row failed",
					"task_id", s.TaskID, "fire_seq", s.FireSeq, "err", err)
				continue
			}
			reaped++
		}
		summary := fmt.Sprintf("reaped %d stuck fires", reaped)
		if failed > 0 {
			summary = fmt.Sprintf("%s (%d append failures)", summary, failed)
		}
		return runner.Outcome{
			Summary: summary,
			Reason:  "reap_stuck",
		}, nil
	}
}
