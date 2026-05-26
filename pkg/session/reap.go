package session

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/hugr-lab/hugen/pkg/scheduler/runner"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

// processOrphanQuietWindow is the minimum quiet period an
// `active` session row must accumulate before [ReapProcessOrphans]
// will retire it. The quiet check looks at the session's event
// log — a session with at least one event newer than `now -
// processOrphanQuietWindow` is treated as still in use,
// regardless of whether the in-memory manager currently has it
// pumping. The append-only `session_events` table is the
// canonical freshness signal; `sessions.updated_at` is NOT used
// because phase-4 keeps the sessions row append-light (only
// status changes touch it, so an actively chatting session can
// have a frozen `updated_at`).
const processOrphanQuietWindow = time.Hour

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
// Two protection layers guard healthy rows:
//
//  1. `live` is the in-process Manager's "session is currently
//     active in memory" oracle; rows for which it reports true
//     are protected unconditionally.
//  2. The event log is consulted via [store.RuntimeStore.ListEvents]
//     with `From = now - processOrphanQuietWindow`. A session that
//     has appended any event inside that window is treated as in
//     use — its goroutine may be stalled or its parent may have
//     not yet resumed it, but the row is recent enough that
//     terminating it would lose work.
//
// agentID scopes the reap to the local agent — multi-agent
// stores keep peers isolated.
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
		cutoff := time.Now().Add(-processOrphanQuietWindow)

		var reaped, skipped int
		for _, row := range rows {
			if _, isLive := liveIDs[row.ID]; isLive {
				continue
			}
			recent, err := st.ListEvents(ctx, row.ID, store.ListEventsOpts{
				From:  cutoff,
				Limit: 1,
			})
			if err != nil {
				logger.Warn("session reap: list recent events failed",
					"session_id", row.ID,
					"err", err,
				)
				continue
			}
			if len(recent) > 0 {
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
