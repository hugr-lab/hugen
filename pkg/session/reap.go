package session

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/hugr-lab/hugen/pkg/scheduler/runner"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

// processOrphanCutoff is the minimum age an `active` session row
// must reach before [ReapProcessOrphans] will retire it. The
// guard keeps the reaper from racing the boot path that loads
// sessions into the live manager but hasn't yet finished
// [manager.Manager.RestoreActive]. Tuned long enough to outlast
// every restart sequence we have observed.
const processOrphanCutoff = time.Hour

// ReapProcessOrphans builds the [runner.RunnerFn] for the
// `sessions_reap_process_orphans` system runner (Phase 6.1a
// §16.1). A session row remains `status='active'` only because
// the process owning it appended no terminate event before
// crashing — graceful shutdown deliberately writes nothing
// (phase-4 invariant, see [manager.Manager.Stop]). The reaper
// flips those rows to `terminated` so dormant operator UIs do
// not list ghost sessions and so subsequent restart walks have a
// clean baseline.
//
// agentID scopes the reap to the local agent — multi-agent
// stores keep peers isolated. live is the in-process Manager's
// "session is currently active in memory" oracle; rows for which
// live reports true are protected unconditionally regardless of
// row age.
func ReapProcessOrphans(agentID string, st store.RuntimeStore, live LiveSessions, logger *slog.Logger) runner.RunnerFn {
	if logger == nil {
		logger = slog.Default()
	}
	return func(ctx context.Context, _ runner.FireMeta) (runner.Outcome, error) {
		if st == nil {
			return runner.Outcome{}, fmt.Errorf("session reap: nil store")
		}
		rows, err := st.ListSessions(ctx, agentID, store.StatusActive)
		if err != nil {
			return runner.Outcome{}, fmt.Errorf("session reap: list active: %w", err)
		}
		if len(rows) == 0 {
			return runner.Outcome{Summary: "no active sessions"}, nil
		}
		liveIDs := liveSet(live)
		cutoff := time.Now().Add(-processOrphanCutoff)

		var reaped, skipped int
		for _, row := range rows {
			if _, isLive := liveIDs[row.ID]; isLive {
				continue
			}
			if !row.UpdatedAt.IsZero() && row.UpdatedAt.After(cutoff) {
				skipped++
				continue
			}
			if err := st.UpdateSessionStatus(ctx, row.ID, store.StatusTerminated); err != nil {
				logger.Warn("session reap: terminate failed",
					"session_id", row.ID,
					"err", err,
				)
				continue
			}
			reaped++
		}
		return runner.Outcome{
			Summary: fmt.Sprintf("reaped=%d skipped=%d total=%d", reaped, skipped, len(rows)),
		}, nil
	}
}

// LiveSessions abstracts the manager.Manager's
// [manager.Manager.SessionsLive] surface so the reaper does not
// take an import cycle on [pkg/session/manager]. The concrete
// type is wired in [pkg/runtime].
type LiveSessions interface {
	SessionsLive() []string
}

func liveSet(live LiveSessions) map[string]struct{} {
	out := map[string]struct{}{}
	if live == nil {
		return out
	}
	for _, id := range live.SessionsLive() {
		out[id] = struct{}{}
	}
	return out
}
