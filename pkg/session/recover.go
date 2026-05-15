package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

// SettleDanglingSubagents is the cross-package entry point used by
// pkg/session/manager.Manager.RestoreActive. Delegates to the
// in-package settleDanglingSubagents so newSessionRestore (which
// also calls settle, internally) stays a single-package
// implementation. The cross-package call passes parent=nil, so the
// phase-5.2 η parked-subagent restore branch is skipped — the
// in-package newSessionRestore path is the one that wires the
// live parent reference required to reattach a parked child.
func SettleDanglingSubagents(ctx context.Context, deps *Deps, parentID string) (int, error) {
	return settleDanglingSubagents(ctx, deps, nil, parentID)
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
// Phase 5.2 η — parked-subagent restore branch. When `parent` is
// non-nil AND the child has no terminal event AND the most recent
// session_status row says `awaiting_dismissal`, the child is
// reattached as a live Session under parent.children (with a fresh
// idle timer) instead of being buried as restart_died. The fresh
// timer reflects the spec's "restart = fresh idle window" decision
// (resolved decision 7). The public SettleDanglingSubagents wrapper
// passes parent=nil so its existing callers (the cross-package
// integration tests) keep their legacy "kill orphaned children"
// semantics.
//
// Idempotent: a second call after a successful first pass observes the
// `subagent_result` rows already on the parent, finds nothing to write,
// and returns 0. Safe to call from `Resume` (per adapter re-attach)
// and from `RestoreActive` (per boot) — the latter uses the return
// value as the active/idle signal (written > 0 → active; 0 → idle, no
// goroutine needed).
//
// `written` counts the number of `subagent_result` rows persisted on
// the parent in this pass. Restored parked children are NOT counted
// because no synthetic row is written — they came back to life.
// Callers use the count as a boot-time activity probe; a parent with
// nothing but restored parked children is still active (the parked
// child registration alone counts at a higher layer).
func settleDanglingSubagents(ctx context.Context, deps *Deps, parent *Session, parentID string) (int, error) {
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

	// Pull only subagent_result rows from the parent's log instead
	// of paging the whole transcript: each parent has at most one
	// such row per child it ever spawned, so the result set is a
	// tiny fraction of the events table.
	parentEvents, err := deps.Store.ListEvents(ctx, parentID, store.ListEventsOpts{
		Kinds: []string{string(protocol.KindSubagentResult)},
	})
	if err != nil {
		return 0, fmt.Errorf("session: settle list-events: %w", err)
	}
	settled := make(map[string]struct{}, len(parentEvents))
	for _, ev := range parentEvents {
		cid, _ := ev.Metadata["session_id"].(string)
		if cid == "" {
			continue
		}
		settled[cid] = struct{}{}
	}

	written := 0
	for _, child := range children {
		// Phase 5.2 η — restore parked children before the legacy
		// "settled subagent_result exists, skip" check. A parked
		// child DOES have a subagent_result on the parent (parking
		// follows a normal completion projection), so the settled
		// gate alone would skip it; we need to reattach the live
		// goroutine and idle timer regardless.
		if parent != nil {
			restored, err := tryRestoreParkedChild(ctx, deps, parent, child.ID)
			if err != nil {
				deps.Logger.Warn("session: settle restore parked child",
					"parent", parentID, "child", child.ID, "err", err)
			}
			if restored {
				continue
			}
		}
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

// appendParentSubagentResult writes a synthetic subagent_result on the
// parent's events for a non-delivering child (recovery path). Reason
// is propagated from the child's terminal (or "restart_died" for
// non-terminal children). Result body is generic — "did not deliver,
// re-spawn if relevant" — so the model gets a clear instruction
// without runtime needing to know skill / role / task. Returns true
// on a successful append.
//
// Live-pump path uses [appendSubagentResultRow] directly with a
// fully-populated SubagentResult constructed from observed child
// frames (real result text + turns count).
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
	return appendSubagentResultRow(ctx, deps, result)
}

// tryRestoreParkedChild reattaches a parked subagent as a live
// Session under parent.children. Returns (true, nil) when the child
// was restored; (false, nil) when the child is not parked (caller
// falls through to the legacy settle branches); (false, err) on a
// store / construction failure (caller logs but continues).
//
// The fresh idle timer + parkedAt stamp encode the "restart = fresh
// idle window" decision (phase 5.2 resolved decision 7). A terminal
// event on the child short-circuits the restore path so a child
// that crashed mid-parking still buries through the legacy
// settle_died flow. Phase 5.2 η.
func tryRestoreParkedChild(ctx context.Context, deps *Deps, parent *Session, childID string) (bool, error) {
	// Terminal-event short-circuit — once the child has its own
	// session_terminated row, it is not parked any more, regardless
	// of intermediate status markers.
	if reason := lookupChildTerminationReason(ctx, deps.Store, childID); reason != "" {
		return false, nil
	}
	row, ok, err := deps.Store.LatestEventOfKinds(ctx, childID, []string{string(protocol.KindSessionStatus)})
	if err != nil {
		return false, fmt.Errorf("latest status: %w", err)
	}
	if !ok {
		return false, nil
	}
	state, _ := row.Metadata["state"].(string)
	if state != protocol.SessionStatusAwaitingDismissal {
		return false, nil
	}
	child, err := newSessionRestore(ctx, childID, parent, deps)
	if err != nil {
		if errors.Is(err, ErrSessionClosed) {
			// Status column has Terminated even though events show
			// awaiting_dismissal — defer to legacy settle which will
			// notice the existing subagent_result and skip.
			return false, nil
		}
		return false, fmt.Errorf("restore session: %w", err)
	}
	// Seed lifecycleState directly — newSessionRestore leaves it at
	// the zero value, and markStatus(awaiting_dismissal) here would
	// re-emit a SessionStatus event identical to the one already in
	// the log. Direct set keeps the events log clean.
	child.statusMu.Lock()
	child.lifecycleState = protocol.SessionStatusAwaitingDismissal
	child.statusMu.Unlock()
	child.parkedAt.Store(time.Now().UnixNano())

	parent.childMu.Lock()
	if parent.children == nil {
		parent.children = map[string]*Session{}
	}
	parent.children[childID] = child
	parent.childMu.Unlock()

	child.Start(ctx)
	parent.childWG.Go(func() { parent.consumeChildOutbox(child) })
	parent.armParkIdleTimer(child)
	if deps.Logger != nil {
		deps.Logger.Info("session: restored parked subagent",
			"parent", parent.id, "child", childID)
	}
	return true, nil
}

// appendSubagentResultRow persists a fully-constructed SubagentResult
// directly to parent's events, bypassing parent.Submit. Used both by
// the recovery wrapper above (for synthetic dangling-child rows) and
// by the live pump's offline-parent fallback (when parent.IsClosed
// would otherwise drop the projection). The frame must already be
// addressed to parent's SessionID with FromSession=child.id.
func appendSubagentResultRow(ctx context.Context, deps *Deps, sr *protocol.SubagentResult) bool {
	row, summary, err := store.FrameToEventRow(sr, deps.Agent.ID())
	if err != nil {
		deps.Logger.Warn("session: project subagent_result row",
			"parent", sr.SessionID(), "child", sr.FromSessionID(), "err", err)
		return false
	}
	if err := deps.Store.AppendEvent(ctx, row, summary); err != nil {
		deps.Logger.Warn("session: append subagent_result row",
			"parent", sr.SessionID(), "child", sr.FromSessionID(), "err", err)
		return false
	}
	return true
}
