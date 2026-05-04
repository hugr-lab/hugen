package session

import (
	"context"
	"fmt"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// Recover settles every sub-agent session that did not append a
// session_terminated event before the previous process exited.
//
// Phase-4 promise: sub-agents do NOT survive process restart in their
// original sessions — instead, dangling sub-agent rows are settled with
// reason "restart_died" and a synthetic subagent_result is appended to
// the parent's events so the parent — when later resumed — sees the
// child as gone instead of waiting for a result that will never come.
//
// In addition to the settle work, Recover collects a danglingRespawn
// per dead sub-agent whose original spawn spec (skill / role / task /
// inputs) can be recovered from the parent's subagent_started event.
// Manager.RestoreActive uses the returned slice to fire fresh
// parent.Spawn calls AFTER each root is alive, so the model sees a
// brand-new child session running the same task instead of having to
// re-spawn manually. Specs are captured BEFORE the synthetic events
// are written so a partial Recover crash doesn't leave the auto-
// respawn pass guessing.
//
// Recover is idempotent: a second call after a successful first one
// is a no-op (every subagent is already terminal) and returns no
// respawns. Safe to call before the manager opens any session, and
// safe to call from a recovery test that re-uses the same store.
//
// Recover does not start any goroutine and does not touch m.live —
// it only writes events. The matching root-restart loop + auto-respawn
// dispatch live in Manager.RestoreActive.
func Recover(ctx context.Context, deps *sessionDeps) ([]danglingRespawn, error) {
	if deps == nil {
		return nil, fmt.Errorf("session: recover requires deps")
	}
	rows, err := deps.store.ListSessions(ctx, deps.agent.ID(), "")
	if err != nil {
		return nil, fmt.Errorf("session: recover list: %w", err)
	}
	var respawns []danglingRespawn
	for _, row := range rows {
		if row.SessionType != "subagent" {
			continue
		}
		if hasTerminated(ctx, deps.store, row.ID) {
			continue
		}
		// Capture the original spawn spec from the parent's
		// subagent_started event BEFORE we write the synthetic
		// terminations. Missing parent or missing source event → no
		// auto-respawn for this row; settle still runs.
		if row.ParentSessionID != "" {
			if spec, ok := lookupSpawnSpec(ctx, deps.store, row.ParentSessionID, row.ID); ok {
				respawns = append(respawns, danglingRespawn{
					parentID: row.ParentSessionID,
					oldID:    row.ID,
					spec:     spec,
				})
			}
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
	return respawns, nil
}

// danglingRespawn is one auto-respawn directive harvested from a
// dangling sub-agent row. Manager.RestoreActive consumes the slice
// after every root is alive: for each entry whose parentID is in
// m.live (i.e. a directly-restored root), it calls parent.Spawn(spec)
// to create a fresh sub-agent with the same task. Sub-agents whose
// parent is NOT a root (grandchildren whose parent was itself a
// dangling sub-agent) intentionally stay dead — their parent was not
// restored, so there is nobody to re-own them.
type danglingRespawn struct {
	parentID string
	oldID    string
	spec     SpawnSpec
}

// lookupSpawnSpec rebuilds a SpawnSpec from the parent's
// subagent_started event for childID. Returns ok=false if the parent
// has no event log, no matching subagent_started row, or the row's
// payload can't be decoded. Best-effort — a missing source event is
// treated as "skip auto-respawn for this child", not an error.
func lookupSpawnSpec(ctx context.Context, store RuntimeStore, parentID, childID string) (SpawnSpec, bool) {
	rows, err := store.ListEvents(ctx, parentID, ListEventsOpts{})
	if err != nil {
		return SpawnSpec{}, false
	}
	for _, r := range rows {
		if r.EventType != string(protocol.KindSubagentStarted) {
			continue
		}
		f, err := EventRowToFrame(r)
		if err != nil {
			continue
		}
		st, ok := f.(*protocol.SubagentStarted)
		if !ok {
			continue
		}
		if st.Payload.ChildSessionID != childID {
			continue
		}
		return SpawnSpec{
			Skill:                  st.Payload.Skill,
			Role:                   st.Payload.Role,
			Task:                   st.Payload.Task,
			Inputs:                 st.Payload.Inputs,
			ParentWhiteboardActive: st.Payload.ParentWhiteboardActive,
		}, true
	}
	return SpawnSpec{}, false
}

// RestoreActive runs at process boot to reattach goroutines for every
// non-terminal root session belonging to this agent. Order:
//
//  1. Recover — settle orphan sub-agents (idempotent) and harvest
//     auto-respawn specs.
//  2. List rows where session_type="root"; for each non-terminal row,
//     call Manager.Resume(ctx, id). Resume re-runs lifecycle.Acquire,
//     emits a session_resumed marker, and starts the goroutine.
//  3. Auto-respawn: for each respawn whose parent_id matches a live
//     root in m.live, call parent.Spawn(spec) creating a brand-new
//     sub-agent session with the original (skill, role, task, inputs).
//     The fresh sub-agent gets the original Task as its first
//     UserMessage, mirroring callSpawnSubagent's contract. A
//     SystemMessage{kind:"spawned_note"} is also emitted on the
//     parent's events so the parent's model — when it next
//     materialises — sees a clean narrative line ("X died, Y respawned
//     for the same task") rather than just an opaque
//     subagent_result{restart_died}.
//
// Sub-agents whose parent is itself a sub-agent (grandchildren) are
// NOT auto-respawned — their parent isn't in m.live. Recover already
// wrote their terminal events; they stay dead.
//
// Errors from individual Resume / Spawn calls are logged but do not
// abort the loop — one corrupt row should not block the rest of the
// agent's sessions from coming back online.
func (m *Manager) RestoreActive(ctx context.Context) error {
	respawns, err := Recover(ctx, m.deps)
	if err != nil {
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
	m.fireAutoRespawns(ctx, respawns)
	return nil
}

// fireAutoRespawns walks the danglingRespawn list returned by Recover
// and, for every entry whose parent is a live root, creates a fresh
// sub-agent via parent.Spawn + Submit-the-task pattern (mirrors the
// live spawn_subagent tool path). Best-effort per entry — a Spawn
// failure surfaces as a Warn log; the rest of the list still gets
// processed. Subagents whose parent was itself a dangling sub-agent
// (grandchildren) are skipped here because their parent isn't in
// m.live.
func (m *Manager) fireAutoRespawns(ctx context.Context, respawns []danglingRespawn) {
	for _, rs := range respawns {
		m.mu.RLock()
		parent, ok := m.live[rs.parentID]
		m.mu.RUnlock()
		if !ok || parent == nil {
			continue
		}
		child, err := parent.Spawn(ctx, rs.spec)
		if err != nil {
			m.logger.Warn("manager: restore-active auto-respawn",
				"parent", rs.parentID, "old_child", rs.oldID, "err", err)
			continue
		}
		// Surface a system_message on the parent's events so the model
		// sees the maneuver in narrative form on its next materialise.
		// The transcript-visible projection lives in
		// pkg/session/replay.go::projectHistory which projects
		// system_message rows into history under "[system: <kind>]"
		// formatting (matching the live visibility filter).
		notice := fmt.Sprintf(
			"sub-agent %s died on process restart (reason: %s); a fresh sub-agent %s was started with the same task.",
			rs.oldID, protocol.TerminationRestartDied, child.ID(),
		)
		sm := protocol.NewSystemMessage(parent.id, parent.agent.Participant(),
			protocol.SystemMessageSpawnedNote, notice)
		if err := parent.emit(ctx, sm); err != nil {
			m.logger.Warn("manager: restore-active respawn system_message",
				"parent", rs.parentID, "old_child", rs.oldID,
				"new_child", child.ID(), "err", err)
		}
		// Deliver the original Task as the new sub-agent's first user
		// message so its run-loop has something to drive a turn off of.
		// Mirrors callSpawnSubagent's behaviour. Empty-task case: skip
		// the Submit so we don't push a pointless empty message.
		if rs.spec.Task != "" {
			first := protocol.NewUserMessage(child.ID(), parent.agent.Participant(), rs.spec.Task)
			if !child.Submit(ctx, first) {
				m.logger.Warn("manager: restore-active auto-respawn first-message rejected",
					"parent", rs.parentID, "child", child.ID())
			}
		}
	}
}
