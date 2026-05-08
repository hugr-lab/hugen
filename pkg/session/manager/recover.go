package manager

import (
	"context"
	"fmt"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// RestoreActive runs at process boot. For every non-terminated root
// session belonging to this agent it reads the lifecycle state
// via a single narrow store query
// ([store.RuntimeStore.LatestEventOfKinds]) — newest
// [protocol.KindSessionStatus] row — and decides whether to bring
// the goroutine up eagerly:
//
//   - idle                                       → lazy. Adapter
//     Resume on demand rebuilds the session via
//     newSessionRestore.
//   - active / wait_subagents / wait_approval /
//     wait_user_input                            → eager. Manager.Resume
//     reattaches a goroutine; the per-session restore path
//     (newSessionRestore) settles dangling sub-agents internally
//     before the goroutine starts, so this method does not call
//     settle directly.
//   - no marker (pre-9.x branch session)         → skip with a
//     warn log. Hard cutover; the branch isn't published, so the
//     only sessions without markers are dev DBs from before this
//     foundation.
//
// Terminated sessions are filtered out at the database by
// [store.RuntimeStore.ListResumableRoots] — the
// `sessions.status` column is now authoritative (the live Session
// flips it from teardown), so a single indexed-column lookup
// replaces the previous events relation filter. Legacy dev DBs
// where the column is "stuck" on Active despite a terminated
// event still surface here; they remain resumable until the next
// live close updates the column.
//
// Sub-agents are NOT restored — the synthetic subagent_result on
// the parent is the contract. The model decides whether to spawn
// a fresh sub-agent for the same task on its next turn (no
// runtime auto-spawn). Phase-4 spec §12.
//
// Errors from individual roots are logged but do not abort the
// loop.
func (m *Manager) RestoreActive(ctx context.Context) error {
	rows, err := m.store.ListResumableRoots(ctx, m.agent.ID())
	if err != nil {
		return fmt.Errorf("manager: restore-active list: %w", err)
	}
	for _, root := range rows {
		row := root.SessionRow
		if len(root.Lifecycle) == 0 {
			m.logger.Warn("manager: restore-active: no lifecycle marker, skipping",
				"session", row.ID)
			continue
		}
		latest := root.Lifecycle[0]
		state, _ := latest.Metadata["state"].(string)
		switch state {
		case "":
			m.logger.Warn("manager: restore-active: malformed lifecycle marker, skipping",
				"session", row.ID)
			continue
		case protocol.SessionStatusIdle:
			// Lazy: stays dormant; adapter Resume on demand.
			continue
		case protocol.SessionStatusActive,
			protocol.SessionStatusWaitSubagents,
			protocol.SessionStatusWaitApproval,
			protocol.SessionStatusWaitUserInput:
			resumed, err := m.Resume(ctx, row.ID)
			if err != nil {
				m.logger.Warn("manager: restore-active resume",
					"session", row.ID, "err", err)
				continue
			}
			// Promote the lifecycle marker out of any stale wait_*
			// state. Without this, a session whose last transition
			// was wait_subagents (and whose children all delivered
			// before the crash so settle had nothing new to write)
			// would loop through eager Resume on every boot — its
			// persisted marker never converging back to active.
			// Guard drops the emit when the marker is already
			// active.
			resumed.MarkStatus(ctx, protocol.SessionStatusActive, "restore_active_resume")
		default:
			m.logger.Warn("manager: restore-active: unknown lifecycle state, skipping",
				"session", row.ID, "state", state)
		}
	}
	return nil
}
