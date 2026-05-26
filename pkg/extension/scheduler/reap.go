// Package scheduler will host the per-session TaskManager
// extension landing in Phase 6.1b. Phase 6.1a parks the package
// stub here so the `task_runs_reap_stuck` system runner has a
// canonical home; today the reaper is a no-op skeleton because
// the `task_runs` table is not yet created.
package scheduler

import (
	"context"

	"github.com/hugr-lab/hugen/pkg/scheduler/runner"
)

// ReapStuckTaskRuns builds the [runner.RunnerFn] skeleton for the
// `task_runs_reap_stuck` system runner (Phase 6.1a §16.1). A
// task_runs row carrying status='in_flight' whose owning cron
// session has already terminated is the canonical "stuck" shape;
// the reaper flips those rows to status='failed' so retention
// policies and operator UIs see a consistent terminal state.
//
// Phase 6.1a ships the registration with the body STUBBED — the
// `task_runs` table lands with Phase 6.1b's TaskStore. Keeping
// the runner registered at 6.1a only validates the resilience-
// reaper wire path; the body becomes load-bearing once the
// table exists. Until then the fn returns a benign Outcome that
// run-log scans render as "n/a".
func ReapStuckTaskRuns() runner.RunnerFn {
	return func(_ context.Context, _ runner.FireMeta) (runner.Outcome, error) {
		return runner.Outcome{
			Summary: "skipped (task_runs table not yet present — Phase 6.1b)",
			Reason:  "not_yet_present",
		}, nil
	}
}
