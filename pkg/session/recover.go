package session

import (
	"context"
	"fmt"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// Recover settles every sub-agent session that did not append a
// session_terminated event before the previous process exited.
//
// Phase-4 promise: sub-agents do NOT survive process restart (root
// sessions do). On boot we therefore walk the persisted session
// graph, mark every dangling sub-agent terminal with reason
// "restart_died", and append a synthetic subagent_result frame to
// each parent's events so the parent — when later resumed — sees the
// child as gone instead of waiting for a result that will never
// come.
//
// Recover is idempotent: a second call after a successful first one
// is a no-op (every subagent is already terminal). Safe to call
// before the manager opens any session, and safe to call from a
// recovery test that re-uses the same store.
//
// Recover does not start any goroutine and does not touch m.live —
// it only writes events. The matching root-restart loop lives in
// Manager.RestoreActive.
func Recover(ctx context.Context, deps *sessionDeps) error {
	if deps == nil {
		return fmt.Errorf("session: recover requires deps")
	}
	rows, err := deps.store.ListSessions(ctx, deps.agent.ID(), "")
	if err != nil {
		return fmt.Errorf("session: recover list: %w", err)
	}
	for _, row := range rows {
		if row.SessionType != "subagent" {
			continue
		}
		if hasTerminated(ctx, deps.store, row.ID) {
			continue
		}
		// Mark the sub-agent terminal first so a re-run that crashes
		// before posting subagent_result still leaves the child in a
		// well-defined state.
		terminal := protocol.NewSessionTerminated(row.ID, deps.agent.Participant(), protocol.SessionTerminatedPayload{
			Reason: protocol.TerminationRestartDied,
		})
		if termRow, summary, perr := FrameToEventRow(terminal, deps.agent.ID()); perr == nil {
			if err := deps.store.AppendEvent(ctx, termRow, summary); err != nil {
				deps.logger.Warn("session: recover append terminal", "session", row.ID, "err", err)
			}
		} else {
			deps.logger.Warn("session: recover project terminal", "session", row.ID, "err", perr)
		}
		// Synthetic subagent_result on the parent's events. fromSession
		// = child id (per NewSubagentResult convention); reason mirrors
		// the child's terminal reason.
		if row.ParentSessionID == "" {
			// Subagent without parent_session_id is a data-shape bug,
			// not a recovery concern — log and skip the parent emit.
			deps.logger.Warn("session: recover subagent missing parent_session_id", "session", row.ID)
			continue
		}
		result := protocol.NewSubagentResult(row.ParentSessionID, row.ID, deps.agent.Participant(),
			protocol.SubagentResultPayload{
				SessionID: row.ID,
				Reason:    protocol.TerminationRestartDied,
			})
		if resRow, summary, perr := FrameToEventRow(result, deps.agent.ID()); perr == nil {
			if err := deps.store.AppendEvent(ctx, resRow, summary); err != nil {
				deps.logger.Warn("session: recover append subagent_result",
					"parent", row.ParentSessionID, "child", row.ID, "err", err)
			}
		} else {
			deps.logger.Warn("session: recover project subagent_result",
				"parent", row.ParentSessionID, "child", row.ID, "err", perr)
		}
	}
	return nil
}

// RestoreActive runs at process boot to reattach goroutines for every
// non-terminal root session belonging to this agent. Order:
//
//  1. Recover — settle orphan sub-agents (idempotent).
//  2. List rows where session_type="root".
//  3. For each row that has no session_terminated event yet, call
//     Manager.Resume(ctx, id). Resume re-runs lifecycle.Acquire,
//     emits a session_resumed marker, and starts the goroutine.
//
// Sub-agents are NOT restored — Recover already wrote their terminal
// events. Adapters that subsequently reconnect to the parent will
// observe the synthetic subagent_result frames from Recover when they
// re-materialise the parent's history.
//
// Errors from individual Resume calls are logged but do not abort the
// loop — one corrupt row should not block the rest of the agent's
// sessions from coming back online.
func (m *Manager) RestoreActive(ctx context.Context) error {
	if err := Recover(ctx, m.deps); err != nil {
		return err
	}
	rows, err := m.store.ListSessions(ctx, m.agent.ID(), "")
	if err != nil {
		return fmt.Errorf("manager: restore-active list: %w", err)
	}
	for _, row := range rows {
		if row.SessionType != "" && row.SessionType != "root" {
			continue
		}
		if hasTerminated(ctx, m.store, row.ID) {
			continue
		}
		if _, err := m.Resume(ctx, row.ID); err != nil {
			m.logger.Warn("manager: restore-active resume", "session", row.ID, "err", err)
		}
	}
	return nil
}
