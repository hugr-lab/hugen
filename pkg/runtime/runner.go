package runtime

import (
	"context"
	"fmt"
	"time"

	"github.com/hugr-lab/hugen/pkg/extension/mission"
	schedext "github.com/hugr-lab/hugen/pkg/extension/scheduler"
	taskext "github.com/hugr-lab/hugen/pkg/extension/task"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/scheduler/runner"
	"github.com/hugr-lab/hugen/pkg/session"
	"github.com/hugr-lab/hugen/pkg/session/manager"
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

	// Phase 6.1c — Bind the scheduler extension to the session
	// manager + runner so InitState (called when sessions open after
	// this phase) can bootstrap user tasks and the fire fns can
	// dispatch wake / spawn fires. The Bind has to run BEFORE
	// svc.Start so a tick that fires immediately after start finds
	// a fully-wired extension.
	if sched := findSchedulerExtension(core); sched != nil && core.Manager != nil {
		sched.Bind(schedulerHost{mgr: core.Manager}, svc)
	}
	// Phase 6.1d — Bind the task extension to the session manager
	// so synthetic `task:<recipe>` calls can resolve the owner
	// session for spawn dispatch. No Runner dependency — task ext
	// only fires through tool calls, not scheduled ticks.
	if te := findTaskExtension(core); te != nil && core.Manager != nil {
		te.Bind(taskHost{mgr: core.Manager})
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

// findSchedulerExtension returns the *schedext.Extension registered
// in phaseExtensions, or nil when scheduler isn't in the slice.
// Helper exists so runner.go doesn't have to import schedext for
// pure type assertions; concentrating it here keeps the Bind site
// readable.
func findSchedulerExtension(core *Core) *schedext.Extension {
	for _, ext := range core.Extensions {
		if s, ok := ext.(*schedext.Extension); ok {
			return s
		}
	}
	return nil
}

// schedulerHost adapts the runtime's *manager.Manager onto the
// narrow [schedext.SessionHost] surface the fire-dispatch path
// needs. Lives in pkg/runtime so the scheduler extension stays
// decoupled from the concrete manager type — the extension only
// sees the interface.
//
// Phase 6.1c shrinks this to Get + Deliver: spawn dispatch uses
// `owner.Spawn(...)` directly off the *Session returned by Get,
// not a host-level Open.
type schedulerHost struct {
	mgr *manager.Manager
}

func (h schedulerHost) Deliver(ctx context.Context, to string, f protocol.Frame) error {
	return h.mgr.Deliver(ctx, to, f)
}

func (h schedulerHost) Get(id string) (*session.Session, bool) {
	return h.mgr.Get(id)
}

// findTaskExtension returns the *taskext.Extension registered in
// phaseExtensions, or nil when task isn't in the slice.
func findTaskExtension(core *Core) *taskext.Extension {
	for _, ext := range core.Extensions {
		if t, ok := ext.(*taskext.Extension); ok {
			return t
		}
	}
	return nil
}

// taskHost adapts the runtime's *manager.Manager onto the narrow
// [taskext.SessionHost] surface — Get only (no Deliver; task ext
// doesn't push frames into the owner session, it spawns subagents).
type taskHost struct {
	mgr *manager.Manager
}

func (h taskHost) Get(id string) (*session.Session, bool) {
	return h.mgr.Get(id)
}
