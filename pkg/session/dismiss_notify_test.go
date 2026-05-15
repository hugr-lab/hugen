package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/internal/fixture"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// TestRequestChildDismiss_Happy parks a spawned child via the
// session lifetime helper (mirrors the handleSubagentResult park
// branch β added) and asserts RequestChildDismiss tears it down
// with reason `subagent_dismissed`. Phase 5.2 γ.
func TestRequestChildDismiss_Happy(t *testing.T) {
	testStore := fixture.NewTestStore()
	parent, cleanup := newTestParent(t, withTestStore(testStore))
	defer cleanup()

	child, err := parent.Spawn(context.Background(), SpawnSpec{Task: "t"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	drainOutboxOnce(parent.Outbox()) // subagent_started

	// Park the child directly — the parking branch in
	// handleSubagentResult is covered by β tests; here we simulate
	// the post-park state so dismiss can be exercised in isolation.
	parent.parkChild(context.Background(), child)
	// markStatus is async-emit; small settle for the lifecycle frame
	// to land in the child's event log before we read it.
	waitForChildStatus(t, child, protocol.SessionStatusAwaitingDismissal, 2*time.Second)

	ok, err := parent.RequestChildDismiss(context.Background(), child.ID())
	if err != nil || !ok {
		t.Fatalf("RequestChildDismiss: ok=%v err=%v", ok, err)
	}
	select {
	case <-child.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("child did not exit within 2s")
	}
	events, _ := testStore.ListEvents(context.Background(), child.ID(), ListEventsOpts{})
	if !containsKindWithReason(events, protocol.KindSessionTerminated, dismissCloseReason) {
		t.Errorf("child terminated with wrong reason; events=%v", kindsWithReasons(events))
	}
}

// TestRequestChildDismiss_NotParked — dismiss on a still-running
// child returns ErrChildNotParked so the slash handler / tool body
// can surface a directed error. Phase 5.2 γ.
func TestRequestChildDismiss_NotParked(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	child, err := parent.Spawn(context.Background(), SpawnSpec{Task: "t"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	_ = child

	ok, err := parent.RequestChildDismiss(context.Background(), child.ID())
	if !errors.Is(err, ErrChildNotParked) {
		t.Errorf("err = %v; want ErrChildNotParked", err)
	}
	if ok {
		t.Errorf("ok=true on non-parked dismiss; want false")
	}
}

// TestRequestChildDismiss_EmptyID rejects the empty case with
// ErrCancelEmptyID — slash handler surfaces it as usage_error.
func TestRequestChildDismiss_EmptyID(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()
	ok, err := parent.RequestChildDismiss(context.Background(), "  ")
	if !errors.Is(err, ErrCancelEmptyID) {
		t.Errorf("err = %v; want ErrCancelEmptyID", err)
	}
	if ok {
		t.Errorf("ok=true on empty id; want false")
	}
}

// TestRequestChildDismiss_UnknownID returns (false, nil) so the
// slash handler surfaces a no_such_session error rather than
// silently confirming a phantom dismiss.
func TestRequestChildDismiss_UnknownID(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()
	ok, err := parent.RequestChildDismiss(context.Background(), "ses-bogus")
	if err != nil {
		t.Errorf("err = %v; want nil", err)
	}
	if ok {
		t.Errorf("ok=true on unknown id; want false")
	}
}

// TestRequestChildNotify_ToParked_Rearm — notify on a parked child
// flips it back to active (synthetic UserMessage → startTurn).
// Phase 5.2 γ.
func TestRequestChildNotify_ToParked_Rearm(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	child, err := parent.Spawn(context.Background(), SpawnSpec{Task: "t"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	drainOutboxOnce(parent.Outbox()) // subagent_started
	parent.parkChild(context.Background(), child)
	waitForChildStatus(t, child, protocol.SessionStatusAwaitingDismissal, 2*time.Second)

	rearmed, err := parent.RequestChildNotify(context.Background(), child.ID(), "keep looking")
	if err != nil {
		t.Fatalf("RequestChildNotify: %v", err)
	}
	if !rearmed {
		t.Error("rearmed=false; want true (target was parked)")
	}
	// The child should pick up the synthetic UserMessage and re-arm.
	// Status transitions: awaiting_dismissal → active (via startTurn).
	// scriptedModel is empty (no chunks), so the turn ends quickly
	// and the child returns to idle — but it leaves awaiting_dismissal
	// at some point. Assert it left the parked state within a beat.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if child.Status() != protocol.SessionStatusAwaitingDismissal {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("child stayed in %q after notify re-arm", child.Status())
}

// TestRequestChildNotify_ToActive_SystemMessage — notify on a
// running child delivers a SystemMessage parent_note (phase 5.1
// path). Phase 5.2 γ regression guard: the parked branch did NOT
// short-circuit non-parked deliveries.
func TestRequestChildNotify_ToActive_SystemMessage(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	child, err := parent.Spawn(context.Background(), SpawnSpec{Task: "t"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	_ = child

	rearmed, err := parent.RequestChildNotify(context.Background(), child.ID(), "hint")
	if err != nil {
		t.Fatalf("RequestChildNotify: %v", err)
	}
	if rearmed {
		t.Error("rearmed=true on active child; want false")
	}
}

// TestRequestChildNotify_EmptyID rejects.
func TestRequestChildNotify_EmptyID(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()
	ok, err := parent.RequestChildNotify(context.Background(), "  ", "x")
	if !errors.Is(err, ErrCancelEmptyID) {
		t.Errorf("err = %v; want ErrCancelEmptyID", err)
	}
	if ok {
		t.Errorf("rearmed=true on empty id; want false")
	}
}

// TestRequestChildNotify_EmptyContent rejects.
func TestRequestChildNotify_EmptyContent(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()
	child, err := parent.Spawn(context.Background(), SpawnSpec{Task: "t"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	_ = child
	ok, err := parent.RequestChildNotify(context.Background(), child.ID(), "")
	if err == nil {
		t.Errorf("err = nil on empty content; want error")
	}
	if ok {
		t.Errorf("rearmed=true on empty content; want false")
	}
}

// waitForChildStatus polls Status() at 10ms ticks until the target
// state is observed or the deadline fires.
func waitForChildStatus(t *testing.T, c *Session, want string, deadline time.Duration) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if c.Status() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("child status %q != %q after %v", c.Status(), want, deadline)
}
