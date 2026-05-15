package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/internal/fixture"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// TestRequestChildCancel_Happy spawns one child, fires
// RequestChildCancel, and asserts the child exits with the
// SubagentCancelPrefix-stamped reason in its session_terminated row.
// Phase 5.1c.cancel-ux — operator-side counterpart of the model-only
// `session:subagent_cancel` tool path.
func TestRequestChildCancel_Happy(t *testing.T) {
	testStore := fixture.NewTestStore()
	parent, cleanup := newTestParent(t, withTestStore(testStore))
	defer cleanup()

	child, err := parent.Spawn(context.Background(), SpawnSpec{Task: "t"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	drainOutboxOnce(parent.Outbox()) // subagent_started

	if err := parent.RequestChildCancel(context.Background(), child.ID(), "/mission"); err != nil {
		t.Fatalf("RequestChildCancel: %v", err)
	}

	select {
	case <-child.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("child did not exit within 2s")
	}

	events, _ := testStore.ListEvents(context.Background(), child.ID(), ListEventsOpts{})
	wanted := protocol.TerminationSubagentCancelPrefix + "/mission"
	if !containsKindWithReason(events, protocol.KindSessionTerminated, wanted) {
		t.Errorf("child terminated with wrong reason; events=%v", kindsWithReasons(events))
	}
}

// TestRequestChildCancel_EmptyID asserts an empty session id is
// rejected with ErrCancelEmptyID — the slash handler relies on this
// to surface a usage_error to the operator instead of silently
// no-op'ing.
func TestRequestChildCancel_EmptyID(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	err := parent.RequestChildCancel(context.Background(), "  ", "x")
	if !errors.Is(err, ErrCancelEmptyID) {
		t.Errorf("got %v, want ErrCancelEmptyID", err)
	}
}

// TestRequestChildCancel_Idempotent — second cancel against the
// same already-terminated child must return nil (no error, no
// panic). Mirrors the tool-side idempotency contract so the
// `/mission` modal's "click cancel twice" UX stays predictable.
func TestRequestChildCancel_Idempotent(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	child, err := parent.Spawn(context.Background(), SpawnSpec{Task: "t"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if err := parent.RequestChildCancel(context.Background(), child.ID(), "first"); err != nil {
		t.Fatalf("first: %v", err)
	}
	<-child.Done()
	if err := parent.RequestChildCancel(context.Background(), child.ID(), "second"); err != nil {
		t.Errorf("second cancel returned err: %v", err)
	}
}

// TestRequestAllChildrenCancel_FansOut spawns three children and
// asserts all three terminate after a single
// RequestAllChildrenCancel call. The returned id slice carries
// every child that received a SessionClose Frame.
func TestRequestAllChildrenCancel_FansOut(t *testing.T) {
	testStore := fixture.NewTestStore()
	parent, cleanup := newTestParent(t, withTestStore(testStore))
	defer cleanup()

	const N = 3
	children := make([]*Session, 0, N)
	for i := 0; i < N; i++ {
		c, err := parent.Spawn(context.Background(), SpawnSpec{Task: "t"})
		if err != nil {
			t.Fatalf("spawn %d: %v", i, err)
		}
		children = append(children, c)
	}

	ids := parent.RequestAllChildrenCancel(context.Background(), "panic_cancel")
	if len(ids) != N {
		t.Errorf("got %d cancelled ids, want %d", len(ids), N)
	}

	deadline := time.After(3 * time.Second)
	for _, c := range children {
		select {
		case <-c.Done():
		case <-deadline:
			t.Fatalf("child %s did not exit within 3s", c.ID())
		}
		events, _ := testStore.ListEvents(context.Background(), c.ID(), ListEventsOpts{})
		wanted := protocol.TerminationSubagentCancelPrefix + "panic_cancel"
		if !containsKindWithReason(events, protocol.KindSessionTerminated, wanted) {
			t.Errorf("child %s wrong reason; events=%v", c.ID(), kindsWithReasons(events))
		}
	}
}

// TestRequestAllChildrenCancel_NoChildren — empty children map
// returns an empty slice, no error, no side effect. The Esc-Esc
// gesture relies on this to surface "no missions to cancel".
func TestRequestAllChildrenCancel_NoChildren(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	ids := parent.RequestAllChildrenCancel(context.Background(), "panic_cancel")
	if len(ids) != 0 {
		t.Errorf("got %d ids on empty parent, want 0", len(ids))
	}
}

// reasonHasCancelPrefix is a small predicate used to keep test
// readers focused on the prefix invariant rather than the exact
// reason suffix in case callers append metadata.
func reasonHasCancelPrefix(reason string) bool {
	return strings.HasPrefix(reason, protocol.TerminationSubagentCancelPrefix)
}

var _ = reasonHasCancelPrefix // reserved for future tests
