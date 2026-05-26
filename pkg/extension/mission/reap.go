package mission

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/hugr-lab/hugen/pkg/scheduler/runner"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

// subagentSessionType matches [pkg/session/constructors.go]'s
// sessionType constant for spawned children. Duplicated here to
// avoid importing pkg/session from a sibling package.
const subagentSessionType = "subagent"

// ReapOrphanSubagents builds the [runner.RunnerFn] for the
// `subagents_reap_orphan_parent` system runner (Phase 6.1a
// §16.1). A subagent's lifetime is bounded by its parent —
// once the parent terminates the child has no one to notify,
// no inbox consumer for its result, and no place for its
// follow-up cascade to land. The reaper cascades the parent's
// terminated state down so the rows reflect the true
// orphaned shape.
//
// The reaper sweeps active subagent rows in a single pass,
// loading parent rows lazily and only when a child's parent is
// not already in the active set. agentID scopes the sweep to
// the local agent.
func ReapOrphanSubagents(agentID string, st store.RuntimeStore, logger *slog.Logger) runner.RunnerFn {
	if logger == nil {
		logger = slog.Default()
	}
	return func(ctx context.Context, _ runner.FireMeta) (runner.Outcome, error) {
		if st == nil {
			return runner.Outcome{}, fmt.Errorf("subagent reap: nil store")
		}
		active, err := st.ListSessions(ctx, agentID, store.StatusActive)
		if err != nil {
			return runner.Outcome{}, fmt.Errorf("subagent reap: list active: %w", err)
		}
		if len(active) == 0 {
			return runner.Outcome{Summary: "no active sessions"}, nil
		}

		activeIDs := make(map[string]struct{}, len(active))
		for _, row := range active {
			activeIDs[row.ID] = struct{}{}
		}

		// parentCache memoizes per-fire parent lookups so a wave
		// of N subagents sharing one terminated parent triggers
		// one LoadSession round-trip, not N.
		parentCache := make(map[string]bool)

		var reaped int
		for _, child := range active {
			if child.SessionType != subagentSessionType {
				continue
			}
			if child.ParentSessionID == "" {
				continue
			}
			if _, parentIsLive := activeIDs[child.ParentSessionID]; parentIsLive {
				continue
			}
			terminated, ok := parentCache[child.ParentSessionID]
			if !ok {
				parentRow, err := st.LoadSession(ctx, child.ParentSessionID)
				if err != nil {
					logger.Warn("subagent reap: load parent failed",
						"child_id", child.ID,
						"parent_id", child.ParentSessionID,
						"err", err,
					)
					continue
				}
				terminated = parentRow.Status == store.StatusTerminated
				parentCache[child.ParentSessionID] = terminated
			}
			if !terminated {
				continue
			}
			if err := st.UpdateSessionStatus(ctx, child.ID, store.StatusTerminated); err != nil {
				logger.Warn("subagent reap: terminate failed",
					"child_id", child.ID,
					"parent_id", child.ParentSessionID,
					"err", err,
				)
				continue
			}
			reaped++
		}
		return runner.Outcome{
			Summary: fmt.Sprintf("reaped=%d total_active=%d", reaped, len(active)),
		}, nil
	}
}
