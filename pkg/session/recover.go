package session

import (
	"context"
	"fmt"

	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

// SettleDanglingSubagents is the cross-package entry point used by
// pkg/session/manager.Manager.RestoreActive. Delegates to the
// in-package settleDanglingSubagents so newSessionRestore (which
// also calls settle, internally) stays a single-package
// implementation.
func SettleDanglingSubagents(ctx context.Context, deps *Deps, parentID string) (int, error) {
	return settleDanglingSubagents(ctx, deps, parentID)
}

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

// lookupChildTerminationReason returns the reason field of the
// child's own `session_terminated` event, or "" if the child has no
// terminal event yet (i.e. it never exited gracefully). Single
// indexed probe via [store.RuntimeStore.LatestEventOfKinds] — no
// full-table walk.
func lookupChildTerminationReason(ctx context.Context, rs store.RuntimeStore, childID string) string {
	row, ok, err := rs.LatestEventOfKinds(ctx, childID, []string{string(protocol.KindSessionTerminated)})
	if err != nil || !ok {
		return ""
	}
	if reason, _ := row.Metadata["reason"].(string); reason != "" {
		return reason
	}
	// Fallback: store.go FrameToEventRow stashes the reason in
	// row.Content for SessionTerminated frames.
	return row.Content
}

// appendChildTerminal best-effort writes session_terminated{reason}
// on the child's events AND flips the child's `sessions.status`
// column to Terminated so list/visibility queries that key off the
// column see the orphan as dead. Symmetric with [Session.handleExit]
// for live sessions: event first (durability), column second (cache).
// Errors are logged; callers continue regardless — the parent-side
// subagent_result still gets written, which is the load-bearing
// piece for the parent's view.
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
	if err := deps.Store.UpdateSessionStatus(ctx, childID, store.StatusTerminated); err != nil {
		deps.Logger.Warn("session: settle update child status",
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
