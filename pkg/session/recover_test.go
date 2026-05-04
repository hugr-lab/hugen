package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// TestSettleDangling_NonTerminalChild_RestartDied: a child row exists,
// has no terminal event, and no subagent_result is on the parent's
// events. Settle must (1) write session_terminated{restart_died} on
// the child's events, (2) write a subagent_result{restart_died} on the
// parent with a clear instruction body, and (3) report written=1.
func TestSettleDangling_NonTerminalChild_RestartDied(t *testing.T) {
	store := newFakeStore()
	ctx := context.Background()
	now := time.Now().UTC()

	parentID, childID := "root1", "sub1"
	mustOpen(t, store, ctx, SessionRow{
		ID: parentID, AgentID: "a1", SessionType: "root", Status: StatusActive,
		CreatedAt: now, UpdatedAt: now,
	})
	mustOpen(t, store, ctx, SessionRow{
		ID: childID, AgentID: "a1", ParentSessionID: parentID,
		SessionType: "subagent", Status: StatusActive,
		Metadata:    map[string]any{"depth": 1},
		CreatedAt:   now, UpdatedAt: now,
	})

	mgr := newTestManager(t, store)
	written, err := settleDanglingSubagents(ctx, mgr.deps, parentID)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if written != 1 {
		t.Fatalf("written = %d, want 1", written)
	}

	if !containsKindWithReason(must(store.ListEvents(ctx, childID, ListEventsOpts{})),
		protocol.KindSessionTerminated, protocol.TerminationRestartDied) {
		t.Errorf("child missing session_terminated{restart_died}")
	}
	parentEvents := must(store.ListEvents(ctx, parentID, ListEventsOpts{}))
	var foundResult bool
	for _, ev := range parentEvents {
		if ev.EventType != string(protocol.KindSubagentResult) {
			continue
		}
		if ev.Metadata["session_id"] != childID {
			continue
		}
		if ev.Metadata["reason"] != protocol.TerminationRestartDied {
			t.Errorf("subagent_result reason = %v, want %s",
				ev.Metadata["reason"], protocol.TerminationRestartDied)
		}
		// Result body is generic but must reference the child id and the reason.
		body := ev.Content
		if body == "" {
			body, _ = ev.Metadata["result"].(string)
		}
		if body == "" {
			t.Errorf("subagent_result has empty Result body")
		}
		foundResult = true
		break
	}
	if !foundResult {
		t.Errorf("parent missing subagent_result for child %s", childID)
	}
}

// TestSettleDangling_CleanlyTerminatedChild_PreservesReason: a child
// has its own session_terminated{completed} event but the
// subagent_result frame was lost in flight (parent.events has none).
// Settle must propagate the child's REAL reason ("completed") into the
// synthetic parent-side subagent_result — never fake "restart_died" for
// a child that exited cleanly.
func TestSettleDangling_CleanlyTerminatedChild_PreservesReason(t *testing.T) {
	store := newFakeStore()
	ctx := context.Background()
	now := time.Now().UTC()

	parentID, childID := "root1", "sub1"
	mustOpen(t, store, ctx, SessionRow{
		ID: parentID, AgentID: "a1", SessionType: "root", Status: StatusActive,
		CreatedAt: now, UpdatedAt: now,
	})
	mustOpen(t, store, ctx, SessionRow{
		ID: childID, AgentID: "a1", ParentSessionID: parentID,
		SessionType: "subagent", Status: StatusActive,
		Metadata:    map[string]any{"depth": 1},
		CreatedAt:   now, UpdatedAt: now,
	})
	// Pre-seed child as cleanly terminated.
	terminal := protocol.NewSessionTerminated(childID,
		protocol.ParticipantInfo{ID: "a1", Kind: protocol.ParticipantAgent},
		protocol.SessionTerminatedPayload{Reason: protocol.TerminationCompleted})
	tRow, tSum, err := FrameToEventRow(terminal, "a1")
	if err != nil {
		t.Fatalf("project terminal: %v", err)
	}
	if err := store.AppendEvent(ctx, tRow, tSum); err != nil {
		t.Fatalf("append child terminal: %v", err)
	}

	mgr := newTestManager(t, store)
	written, err := settleDanglingSubagents(ctx, mgr.deps, parentID)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if written != 1 {
		t.Fatalf("written = %d, want 1", written)
	}

	parentEvents := must(store.ListEvents(ctx, parentID, ListEventsOpts{}))
	var sawCorrectReason bool
	for _, ev := range parentEvents {
		if ev.EventType != string(protocol.KindSubagentResult) {
			continue
		}
		if ev.Metadata["session_id"] != childID {
			continue
		}
		if ev.Metadata["reason"] == protocol.TerminationCompleted {
			sawCorrectReason = true
		} else {
			t.Errorf("subagent_result reason = %v, want %s (clean exit must preserve)",
				ev.Metadata["reason"], protocol.TerminationCompleted)
		}
	}
	if !sawCorrectReason {
		t.Errorf("parent missing subagent_result{completed} for cleanly-terminated child")
	}
	// The child's existing session_terminated{completed} must NOT be
	// overwritten by a synthetic restart_died (idempotent on terminal).
	childEvents := must(store.ListEvents(ctx, childID, ListEventsOpts{}))
	termCount := 0
	for _, ev := range childEvents {
		if ev.EventType == string(protocol.KindSessionTerminated) {
			termCount++
		}
	}
	if termCount != 1 {
		t.Errorf("child has %d session_terminated events, want exactly 1", termCount)
	}
}

// TestSettleDangling_AlreadySettled_NoOp: parent already has a
// subagent_result for the child. Settle must NOT write anything more —
// neither on parent nor on child — and report written=0.
func TestSettleDangling_AlreadySettled_NoOp(t *testing.T) {
	store := newFakeStore()
	ctx := context.Background()
	now := time.Now().UTC()

	parentID, childID := "root1", "sub1"
	mustOpen(t, store, ctx, SessionRow{
		ID: parentID, AgentID: "a1", SessionType: "root", Status: StatusActive,
		CreatedAt: now, UpdatedAt: now,
	})
	mustOpen(t, store, ctx, SessionRow{
		ID: childID, AgentID: "a1", ParentSessionID: parentID,
		SessionType: "subagent", Status: StatusActive,
		Metadata:    map[string]any{"depth": 1},
		CreatedAt:   now, UpdatedAt: now,
	})
	// Pre-seed the parent with an existing subagent_result.
	pre := protocol.NewSubagentResult(parentID, childID,
		protocol.ParticipantInfo{ID: "a1", Kind: protocol.ParticipantAgent},
		protocol.SubagentResultPayload{
			SessionID: childID,
			Reason:    protocol.TerminationCompleted,
			Result:    "ok",
		})
	row, sum, err := FrameToEventRow(pre, "a1")
	if err != nil {
		t.Fatalf("project pre-result: %v", err)
	}
	if err := store.AppendEvent(ctx, row, sum); err != nil {
		t.Fatalf("append pre-result: %v", err)
	}

	mgr := newTestManager(t, store)
	written, err := settleDanglingSubagents(ctx, mgr.deps, parentID)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if written != 0 {
		t.Errorf("written = %d on already-settled parent, want 0", written)
	}
	// Child must NOT have gained a session_terminated row.
	for _, ev := range must(store.ListEvents(ctx, childID, ListEventsOpts{})) {
		if ev.EventType == string(protocol.KindSessionTerminated) {
			t.Errorf("idempotent settle wrote a child terminal: %v", ev)
		}
	}
}

// TestRestoreActive_SkipsIdleRoot: a root with no children is idle.
// RestoreActive must not bring up its goroutine (no entry in
// SessionsLive) — adapter Resume on demand handles it later.
func TestRestoreActive_SkipsIdleRoot(t *testing.T) {
	store := newFakeStore()
	ctx := context.Background()
	now := time.Now().UTC()

	mustOpen(t, store, ctx, SessionRow{
		ID: "root_idle", AgentID: "a1", SessionType: "root", Status: StatusActive,
		CreatedAt: now, UpdatedAt: now,
	})

	mgr := newTestManager(t, store)
	if err := mgr.RestoreActive(ctx); err != nil {
		t.Fatalf("RestoreActive: %v", err)
	}
	defer mgr.ShutdownAll(ctx)

	if live := mgr.SessionsLive(); len(live) != 0 {
		t.Errorf("idle root brought up at boot: live=%v", live)
	}
}

// TestRestoreActive_RestoresActiveRoot: a root with one dangling
// non-terminal child is active. RestoreActive must (1) settle the
// child, (2) bring up the root's goroutine.
func TestRestoreActive_RestoresActiveRoot(t *testing.T) {
	store := newFakeStore()
	ctx := context.Background()
	now := time.Now().UTC()

	mustOpen(t, store, ctx, SessionRow{
		ID: "root_active", AgentID: "a1", SessionType: "root", Status: StatusActive,
		CreatedAt: now, UpdatedAt: now,
	})
	mustOpen(t, store, ctx, SessionRow{
		ID: "sub1", AgentID: "a1", ParentSessionID: "root_active",
		SessionType: "subagent", Status: StatusActive,
		Metadata:    map[string]any{"depth": 1},
		CreatedAt:   now, UpdatedAt: now,
	})

	mgr := newTestManager(t, store)
	if err := mgr.RestoreActive(ctx); err != nil {
		t.Fatalf("RestoreActive: %v", err)
	}
	defer mgr.ShutdownAll(ctx)

	live := mgr.SessionsLive()
	if len(live) != 1 || live[0] != "root_active" {
		t.Errorf("active root not in SessionsLive: live=%v", live)
	}

	// Settle wrote a subagent_result for the dangling child.
	parentEvents := must(store.ListEvents(ctx, "root_active", ListEventsOpts{}))
	var saw bool
	for _, ev := range parentEvents {
		if ev.EventType == string(protocol.KindSubagentResult) &&
			ev.Metadata["session_id"] == "sub1" &&
			ev.Metadata["reason"] == protocol.TerminationRestartDied {
			saw = true
			break
		}
	}
	if !saw {
		t.Errorf("active root missing settle subagent_result for sub1")
	}
}

// TestResume_RejectsSubAgentID confirms the Manager.Resume invariant
// that m.live is root-only (phase-4-tree-ctx-routing ADR D4): passing
// a sub-agent id surfaces ErrNotRootSession instead of registering
// the row in m.live as a fake root. Sub-agents are reachable only
// through their parent's children map.
func TestResume_RejectsSubAgentID(t *testing.T) {
	store := newFakeStore()
	ctx := context.Background()
	now := time.Now().UTC()

	mustOpen(t, store, ctx, SessionRow{
		ID: "root1", AgentID: "a1", SessionType: "root", Status: StatusActive,
		CreatedAt: now, UpdatedAt: now,
	})
	mustOpen(t, store, ctx, SessionRow{
		ID: "sub1", AgentID: "a1", ParentSessionID: "root1",
		SessionType: "subagent", Status: StatusActive,
		Metadata:    map[string]any{"depth": 1},
		CreatedAt:   now, UpdatedAt: now,
	})

	mgr := newTestManager(t, store)
	defer mgr.ShutdownAll(ctx)

	if _, err := mgr.Resume(ctx, "sub1"); err == nil {
		t.Fatalf("Resume(subagent) returned nil error, want ErrNotRootSession")
	} else if !errors.Is(err, ErrNotRootSession) {
		t.Errorf("Resume(subagent) err = %v, want to wrap ErrNotRootSession", err)
	}
	for _, id := range mgr.SessionsLive() {
		if id == "sub1" {
			t.Errorf("sub-agent leaked into m.live after rejected Resume")
		}
	}
}

// TestRestoreActive_SkipsTerminalRoots: a root with its own
// session_terminated event is dead. RestoreActive must skip it,
// even if it had children (which stay as orphan rows in DB —
// no parent ever wakes up to read them).
func TestRestoreActive_SkipsTerminalRoots(t *testing.T) {
	store := newFakeStore()
	ctx := context.Background()
	now := time.Now().UTC()

	mustOpen(t, store, ctx, SessionRow{
		ID: "root_dead", AgentID: "a1", SessionType: "root", Status: StatusActive,
		CreatedAt: now, UpdatedAt: now,
	})
	dead := protocol.NewSessionTerminated("root_dead",
		protocol.ParticipantInfo{ID: "a1", Kind: protocol.ParticipantAgent},
		protocol.SessionTerminatedPayload{Reason: protocol.TerminationUserEnd})
	dRow, dSum, _ := FrameToEventRow(dead, "a1")
	_ = store.AppendEvent(ctx, dRow, dSum)

	mgr := newTestManager(t, store)
	if err := mgr.RestoreActive(ctx); err != nil {
		t.Fatalf("RestoreActive: %v", err)
	}
	defer mgr.ShutdownAll(ctx)

	if live := mgr.SessionsLive(); len(live) != 0 {
		t.Errorf("terminal root resurrected: live=%v", live)
	}
}

// helpers

func mustOpen(t *testing.T, store RuntimeStore, ctx context.Context, row SessionRow) {
	t.Helper()
	if err := store.OpenSession(ctx, row); err != nil {
		t.Fatalf("OpenSession %s: %v", row.ID, err)
	}
}

func must(events []EventRow, _ error) []EventRow { return events }

func containsKindWithReason(events []EventRow, kind protocol.Kind, reason string) bool {
	for _, ev := range events {
		if ev.EventType == string(kind) {
			if r, _ := ev.Metadata["reason"].(string); r == reason {
				return true
			}
		}
	}
	return false
}

