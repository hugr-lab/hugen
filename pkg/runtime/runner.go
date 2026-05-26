package runtime

import (
	"context"
	"fmt"
	"time"

	"github.com/hugr-lab/hugen/pkg/extension/mission"
	schedext "github.com/hugr-lab/hugen/pkg/extension/scheduler"
	"github.com/hugr-lab/hugen/pkg/scheduler/runner"
	"github.com/hugr-lab/hugen/pkg/session"
)

// phaseRunner runs phase 10: builds the agent-level scheduling
// runner (Phase 6.1a) and registers the always-on resilience
// reapers catalogued in design/004-runtime-post-phase-i/
// phase-6-spec.md §16.1. Lives downstream of phaseSessionManager
// so the session reaper can consult the Manager's
// SessionsLive() oracle when classifying orphan rows.
//
// Phase 6.1a only registers system runners. Per-session
// TaskManager extensions (Phase 6.1b) will re-register their
// user-task fns at session bootstrap; the Runner here is the
// shared host.
func phaseRunner(ctx context.Context, core *Core) error {
	svc := runner.New(runner.WithLogger(core.Logger))

	if err := svc.Register(ctx,
		"sessions_reap_process_orphans",
		runner.Every(time.Hour),
		session.ReapProcessOrphans(core.Agent.ID(), core.Store, core.Manager, core.Logger),
	); err != nil {
		return fmt.Errorf("register sessions_reap_process_orphans: %w", err)
	}

	if err := svc.Register(ctx,
		"subagents_reap_orphan_parent",
		runner.Every(time.Hour),
		mission.ReapOrphanSubagents(core.Agent.ID(), core.Store, core.Logger),
	); err != nil {
		return fmt.Errorf("register subagents_reap_orphan_parent: %w", err)
	}

	// Phase 6.1b — `task_log_reap_stuck` (renamed from the 6.1a
	// stub `task_runs_reap_stuck`). The body now drives against
	// the real `task_log` table and INSERTs `cancelled` rows
	// append-only when a fire's `started` row sits without a
	// terminal match past the cutoff.
	if core.TaskStore != nil {
		if err := svc.Register(ctx,
			"task_log_reap_stuck",
			runner.Every(time.Hour),
			schedext.ReapStuckTaskRuns(core.Agent.ID(), core.TaskStore, core.Logger),
		); err != nil {
			return fmt.Errorf("register task_log_reap_stuck: %w", err)
		}
	}

	if err := svc.Start(ctx); err != nil {
		return fmt.Errorf("start runner: %w", err)
	}

	core.Runner = svc
	core.addCleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := svc.Stop(shutdownCtx); err != nil {
			core.Logger.Warn("runner shutdown timed out", "err", err)
		}
	})
	return nil
}
