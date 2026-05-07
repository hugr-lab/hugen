package session

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/internal/fixture"
)

// TestSubagent_CancelCascade_TwoDeep exercises phase-4-spec §13.2 #5: a
// `/cancel all` on the root cascades through every descendant. Tree
// shape: root → mid → leaf. After the cancel, both mid and leaf
// terminate with reason "cancel_cascade"; root itself does NOT
// terminate (only its in-flight turn would abort, but in this test
// it has no turn — we just assert root is still alive).
func TestSubagent_CancelCascade_TwoDeep(t *testing.T) {
	store := fixture.NewTestStore()
	mgr := newTestManager(t, store)
	ctx := context.Background()
	defer mgr.Stop(ctx)

	root := us1OpenParent(t, mgr)

	mid, err := root.Spawn(ctx, SpawnSpec{Task: "mid"})
	if err != nil {
		t.Fatalf("spawn mid: %v", err)
	}
	drainOutboxOnce(root.Outbox()) // subagent_started{mid}

	leaf, err := mid.Spawn(ctx, SpawnSpec{Task: "leaf"})
	if err != nil {
		t.Fatalf("spawn leaf: %v", err)
	}
	drainOutboxOnce(mid.Outbox()) // subagent_started{leaf}

	// Submit /cancel all to root via Submit so routeInbound's Cancel
	// branch handles it (the branch lives on the Run goroutine).
	cancelFrame := protocol.NewCancel(root.id,
		protocol.ParticipantInfo{ID: "u1", Kind: protocol.ParticipantUser},
		"global stop")
	cancelFrame.Payload.Cascade = true
	if !root.Submit(ctx, cancelFrame) {
		t.Fatal("Submit /cancel all rejected by root")
	}

	// Mid + leaf should both terminate. Root stays alive.
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()

	for _, c := range []*Session{leaf, mid} {
		select {
		case <-c.Done():
		case <-deadline.C:
			t.Fatalf("descendant %q did not exit on cascade", c.id)
		}
	}
	if root.IsClosed() {
		t.Errorf("root unexpectedly closed on /cancel all (expected turn-only abort)")
	}

	for _, c := range []*Session{mid, leaf} {
		evs, _ := store.ListEvents(ctx, c.id, ListEventsOpts{})
		if !containsKindWithReason(evs, protocol.KindSessionTerminated, protocol.TerminationCancelCascade) {
			t.Errorf("session %q termination reason ≠ cancel_cascade: events=%v",
				c.id, kindsWithReasons(evs))
		}
	}
}

// TestSubagent_Result_DeliveredToParent verifies the producer-
// side of the wait_subagents contract: when a child terminates via
// subagent_cancel, a SubagentResult Frame surfaces on the parent —
// either consumed by an active wait_subagents (live path) or
// persisted into parent's events (offline / buffered path).
//
// This test exercises the buffered path: cancel the child, assert
// parent's events end up with a subagent_result row whose
// session_id matches the cancelled child.
func TestSubagent_Result_DeliveredToParent(t *testing.T) {
	store := fixture.NewTestStore()
	mgr := newTestManager(t, store)
	ctx := context.Background()
	defer mgr.Stop(ctx)

	parent := us1OpenParent(t, mgr)
	child, err := parent.Spawn(ctx, SpawnSpec{Task: "t"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	drainOutboxOnce(parent.Outbox())

	args, _ := json.Marshal(subagentCancelInput{
		SessionID: child.id, Reason: "test bail",
	})
	if _, err := parent.callSubagentCancel(us1WithSession(parent), args); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	<-child.Done()

	// The SubagentResult was Submit-ed to parent's inbox; the parent's
	// idle Run loop processes it via routeInbound's RouteBuffered
	// branch — which emits pass-through (persists to events) when no
	// turn is in flight. Wait briefly for that emit to land.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		evs, _ := store.ListEvents(ctx, parent.id, ListEventsOpts{})
		if containsKindForChild(evs, protocol.KindSubagentResult, child.id) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	evs, _ := store.ListEvents(ctx, parent.id, ListEventsOpts{})
	t.Errorf("parent never observed subagent_result for child %q; events=%v",
		child.id, kindsWithReasons(evs))
}

// TestSubagent_Wait_NaturalTermination drives a real cancel-then-
// wait flow: spawn a child, start wait_subagents, then cancel the
// child from a separate goroutine. wait_subagents must return when
// the child's natural exit-time SubagentResult arrives via the
// activeToolFeed.
func TestSubagent_Wait_NaturalTermination(t *testing.T) {
	mgr := newTestManager(t, fixture.NewTestStore())
	ctx := context.Background()
	defer mgr.Stop(ctx)

	parent := us1OpenParent(t, mgr)
	child, err := parent.Spawn(ctx, SpawnSpec{Task: "t"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	drainOutboxOnce(parent.Outbox())

	type res struct {
		out []byte
		err error
	}
	done := make(chan res, 1)
	args, _ := json.Marshal(waitSubagentsInput{IDs: []string{child.id}})
	go func() {
		out, err := parent.callWaitSubagents(us1WithSession(parent), args)
		done <- res{out: out, err: err}
	}()

	// Wait for the feed to register, then cancel the child.
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for parent.activeToolFeed.Load() == nil {
		select {
		case <-time.After(20 * time.Millisecond):
		case <-deadline.C:
			t.Fatal("activeToolFeed never registered")
		}
	}
	// Drive the cancel through the public sub-agent API. mgr.Terminate
	// addresses live-roots only post pivot 4 — sub-agents belong to
	// their parent's children map, so callSubagentCancel is the right
	// path (direct caller.children[id] lookup + child.terminate).
	go func() {
		args, _ := json.Marshal(subagentCancelInput{
			SessionID: child.id, Reason: "natural-test-cancel",
		})
		if _, err := parent.callSubagentCancel(us1WithSession(parent), args); err != nil {
			t.Errorf("cancel: %v", err)
		}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("wait err: %v", r.err)
		}
		var rows []waitResultRow
		if err := json.Unmarshal(r.out, &rows); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(rows) != 1 || rows[0].SessionID != child.id {
			t.Errorf("rows = %+v", rows)
		}
		wantReason := protocol.TerminationSubagentCancelPrefix + "natural-test-cancel"
		if rows[0].Reason != wantReason {
			t.Errorf("row.reason = %q, want %q", rows[0].Reason, wantReason)
		}
		if rows[0].Status != "subagent_cancel" {
			t.Errorf("row.status = %q, want subagent_cancel", rows[0].Status)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("wait_subagents did not return after natural child termination")
	}
}

// containsKindForChild scans events for a row whose EventType matches
// kind AND whose payload (or __from_session metadata) names childID.
// Used to confirm subagent_result delivery without conflating it
// with the SessionTerminated row written on the child itself.
func containsKindForChild(events []EventRow, kind protocol.Kind, childID string) bool {
	for _, ev := range events {
		if ev.EventType != string(kind) {
			continue
		}
		if v, ok := ev.Metadata["session_id"].(string); ok && v == childID {
			return true
		}
		if v, ok := ev.Metadata["__from_session"].(string); ok && v == childID {
			return true
		}
	}
	return false
}
