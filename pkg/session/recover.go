package session

import (
	"context"
	"fmt"

	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

// settleDanglingSubagents reconciles a parent session's child set with
// its events at restore time. For every row in
// `store.ListChildren(parentID)` whose corresponding `subagent_result`
// is missing on the parent's events, this function:
//
//   - if the child has its own `session_terminated` event already
//     (clean exit whose result frame was lost in flight): writes a
//     synthetic `subagent_result` on the parent carrying the child's
//     real terminal reason (so we don't fake `restart_died` for a child
//     that completed cleanly).
//   - if the child is non-terminal (subagent process died with parent
//     on `kill -9`): appends `session_terminated{reason:"restart_died"}`
//     to the child's events first, then writes the synthetic
//     `subagent_result{reason:"restart_died"}` on the parent.
//
// Either branch surfaces a clear `Result` text so the parent's model,
// when it next materialises, sees an explicit instruction rather than
// an opaque terminal row. The model decides on its next turn whether
// to re-spawn — the runtime never auto-spawns.
//
// Idempotent: a second call after a successful first pass observes the
// `subagent_result` rows already on the parent, finds nothing to write,
// and returns 0. Safe to call from `Resume` (per adapter re-attach)
// and from `RestoreActive` (per boot) — the latter uses the return
// value as the active/idle signal (written > 0 → active; 0 → idle, no
// goroutine needed).
//
// `written` counts the number of `subagent_result` rows persisted on
// the parent in this pass — exactly the count of children that needed
// settle, regardless of whether the child was clean-terminated or not.
// Callers use it as boot-time activity probe.
func settleDanglingSubagents(ctx context.Context, deps *Deps, parentID string) (int, error) {
	if deps == nil {
		return 0, fmt.Errorf("session: settle requires deps")
	}
	children, err := deps.Store.ListChildren(ctx, parentID)
	if err != nil {
		return 0, fmt.Errorf("session: settle list-children: %w", err)
	}
	if len(children) == 0 {
		return 0, nil
	}

	parentEvents, err := deps.Store.ListEvents(ctx, parentID, store.ListEventsOpts{})
	if err != nil {
		return 0, fmt.Errorf("session: settle list-events: %w", err)
	}
	settled := make(map[string]struct{})
	for _, ev := range parentEvents {
		if ev.EventType != string(protocol.KindSubagentResult) {
			continue
		}
		cid, _ := ev.Metadata["session_id"].(string)
		if cid == "" {
			continue
		}
		settled[cid] = struct{}{}
	}

	written := 0
	for _, child := range children {
		if _, ok := settled[child.ID]; ok {
			continue
		}
		reason := lookupChildTerminationReason(ctx, deps.Store, child.ID)
		if reason == "" {
			// Non-terminal child — bury it with restart_died first so a
			// future read sees a coherent "child gone" state regardless
			// of whether the parent's row gets written below.
			reason = protocol.TerminationRestartDied
			appendChildTerminal(ctx, deps, child.ID, protocol.TerminationRestartDied)
		}
		if appendParentSubagentResult(ctx, deps, parentID, child.ID, reason) {
			written++
		}
	}
	return written, nil
}

// lookupChildTerminationReason returns the reason field of the child's
// own `session_terminated` event, or "" if the child has no terminal
// event yet (i.e. it never exited gracefully). Reads only — no writes.
func lookupChildTerminationReason(ctx context.Context, rs store.RuntimeStore, childID string) string {
	rows, err := rs.ListEvents(ctx, childID, store.ListEventsOpts{Limit: 1000})
	if err != nil {
		return ""
	}
	for _, r := range rows {
		if r.EventType != string(protocol.KindSessionTerminated) {
			continue
		}
		if reason, _ := r.Metadata["reason"].(string); reason != "" {
			return reason
		}
		// Fallback: store.go FrameToEventRow stashes the reason in
		// row.Content for SessionTerminated frames.
		if r.Content != "" {
			return r.Content
		}
	}
	return ""
}

// appendChildTerminal best-effort writes session_terminated{reason} on
// the child's events. Errors are logged; callers continue regardless
// (the parent-side subagent_result still gets written, which is the
// load-bearing piece for the parent's view).
func appendChildTerminal(ctx context.Context, deps *Deps, childID, reason string) {
	terminal := protocol.NewSessionTerminated(childID, deps.Agent.Participant(),
		protocol.SessionTerminatedPayload{Reason: reason})
	row, summary, err := store.FrameToEventRow(terminal, deps.Agent.ID())
	if err != nil {
		deps.Logger.Warn("session: settle project child terminal",
			"child", childID, "err", err)
		return
	}
	if err := deps.Store.AppendEvent(ctx, row, summary); err != nil {
		deps.Logger.Warn("session: settle append child terminal",
			"child", childID, "err", err)
	}
}

// appendParentSubagentResult writes the synthetic subagent_result on
// the parent's events. Reason is propagated from the child's terminal
// (or "restart_died" for non-terminal children). Result text is
// generic — "did not deliver, re-spawn if relevant" — so the model
// gets a clear instruction without runtime needing to know skill /
// role / task. Returns true on a successful append.
func appendParentSubagentResult(ctx context.Context, deps *Deps, parentID, childID, reason string) bool {
	body := fmt.Sprintf(
		"Sub-agent %s did not deliver a result before the previous process exited (reason: %s). If the work is still relevant, re-spawn a fresh sub-agent for it.",
		childID, reason,
	)
	result := protocol.NewSubagentResult(parentID, childID, deps.Agent.Participant(),
		protocol.SubagentResultPayload{
			SessionID: childID,
			Reason:    reason,
			Result:    body,
		})
	row, summary, err := store.FrameToEventRow(result, deps.Agent.ID())
	if err != nil {
		deps.Logger.Warn("session: settle project subagent_result",
			"parent", parentID, "child", childID, "err", err)
		return false
	}
	if err := deps.Store.AppendEvent(ctx, row, summary); err != nil {
		deps.Logger.Warn("session: settle append subagent_result",
			"parent", parentID, "child", childID, "err", err)
		return false
	}
	return true
}

// RestoreActive runs at process boot. For every non-terminal root
// session belonging to this agent it reads the lifecycle state
// from the persisted [protocol.KindSessionStatus] markers and
// decides whether to bring the goroutine up eagerly:
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
// loop.
func (m *Manager) RestoreActive(ctx context.Context) error {
	rows, err := m.store.ListSessions(ctx, m.agent.ID(), "")
	if err != nil {
		return fmt.Errorf("manager: restore-active list: %w", err)
	}
	for _, row := range rows {
		if row.SessionType != "" && row.SessionType != "root" {
			continue
		}
		events, err := m.store.ListEvents(ctx, row.ID, store.ListEventsOpts{})
		if err != nil {
			m.logger.Warn("manager: restore-active list events",
				"session", row.ID, "err", err)
			continue
		}
		if hasTerminatedRows(events) {
			continue
		}
		state := lookupLatestStatusEvent(events)
		switch state {
		case "":
			m.logger.Warn("manager: restore-active: no lifecycle marker, skipping",
				"session", row.ID)
			continue
		case protocol.SessionStatusIdle:
			// Lazy: stays dormant; adapter Resume on demand.
			continue
		case protocol.SessionStatusActive,
			protocol.SessionStatusWaitSubagents,
			protocol.SessionStatusWaitApproval,
			protocol.SessionStatusWaitUserInput:
			if _, err := settleDanglingSubagents(ctx, m.deps, row.ID); err != nil {
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
			resumed.markStatus(ctx, protocol.SessionStatusActive, "restore_active_resume")
		default:
			m.logger.Warn("manager: restore-active: unknown lifecycle state, skipping",
				"session", row.ID, "state", state)
		}
	}
	return nil
}

// hasTerminatedRows is the events-slice variant of hasTerminated —
// avoids a second store round-trip when the caller already loaded
// the full log (RestoreActive does).
func hasTerminatedRows(events []store.EventRow) bool {
	for _, ev := range events {
		if ev.EventType == string(protocol.KindSessionTerminated) {
			return true
		}
	}
	return false
}
