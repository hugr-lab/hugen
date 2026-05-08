package manager

import (
	"fmt"

	"context"

	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session"
)

// RestoreActive runs at process boot. For every non-terminal root
// session belonging to this agent it reads the lifecycle state
// via a single narrow store query
// ([store.RuntimeStore.LatestEventOfKinds]) — newest matched row
// of [protocol.KindSessionTerminated, protocol.KindSessionStatus]
// — and decides whether to bring the goroutine up eagerly:
//
//   - idle                                       → lazy. Adapter
//     Resume on demand rebuilds the session via
//     newSessionRestore.
//   - active / wait_subagents / wait_approval /
//     wait_user_input                            → eager. Settle
//     dangling sub-agents (writes synthetic subagent_result on
//     the parent for children whose result didn't land last run)
//     then Manager.Resume reattaches a goroutine.
//   - no marker (pre-9.x branch session)         → skip with a
//     warn log. Hard cutover; the branch isn't published, so the
//     only sessions without markers are dev DBs from before this
//     foundation.
//
// Sub-agents are NOT restored — the synthetic subagent_result on
// the parent is the contract. The model decides whether to spawn
// a fresh sub-agent for the same task on its next turn (no
// runtime auto-spawn). Phase-4 spec §12.
//
// Errors from individual roots are logged but do not abort the
// loop. The dangling-subagent settle helpers themselves live in
// pkg/session/recover.go (so newSessionRestore can call them
// internally) — this method delegates via session.SettleDanglingSubagents.
func (m *Manager) RestoreActive(ctx context.Context) error {
	rows, err := m.store.ListSessions(ctx, m.agent.ID(), "")
	if err != nil {
		return fmt.Errorf("manager: restore-active list: %w", err)
	}
	probeKinds := []string{
		string(protocol.KindSessionTerminated),
		string(protocol.KindSessionStatus),
	}
	for _, row := range rows {
		if row.SessionType != "" && row.SessionType != "root" {
			continue
		}
		latest, ok, err := m.store.LatestEventOfKinds(ctx, row.ID, probeKinds)
		if err != nil {
			m.logger.Warn("manager: restore-active probe",
				"session", row.ID, "err", err)
			continue
		}
		if !ok {
			m.logger.Warn("manager: restore-active: no lifecycle marker, skipping",
				"session", row.ID)
			continue
		}
		if protocol.Kind(latest.EventType) == protocol.KindSessionTerminated {
			continue
		}
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
			if _, err := session.SettleDanglingSubagents(ctx, m.deps, row.ID); err != nil {
				m.logger.Warn("manager: restore-active settle",
					"session", row.ID, "err", err)
				continue
			}
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
