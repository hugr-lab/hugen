package session

import (
	"context"
	"errors"
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

	if ok, err := parent.RequestChildCancel(context.Background(), child.ID(), "/mission"); err != nil || !ok {
		t.Fatalf("RequestChildCancel: ok=%v err=%v", ok, err)
	}

	select {
	case <-child.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("child did not exit within 2s")
	}

	events, _ := testStore.ListEvents(context.Background(), child.ID(), ListEventsOpts{})
	wanted := protocol.TerminationUserCancelPrefix + "/mission"
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

	ok, err := parent.RequestChildCancel(context.Background(), "  ", "x")
	if !errors.Is(err, ErrCancelEmptyID) {
		t.Errorf("got %v, want ErrCancelEmptyID", err)
	}
	if ok {
		t.Errorf("ok=true on empty id; want false")
	}
}

// TestRequestChildCancel_UnknownIDReportsNotCancelled — operator
// typo case: an id that is not in parent.children returns
// (false, nil) so the slash handler can surface a usage error
// rather than confirming a phantom cancel. Phase 5.1c.cancel-ux
// review fix.
func TestRequestChildCancel_UnknownIDReportsNotCancelled(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	ok, err := parent.RequestChildCancel(context.Background(), "ses-typo123", "x")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ok {
		t.Errorf("ok=true for unknown id; want false")
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
	if ok, err := parent.RequestChildCancel(context.Background(), child.ID(), "first"); err != nil || !ok {
		t.Fatalf("first: ok=%v err=%v", ok, err)
	}
	<-child.Done()
	ok, err := parent.RequestChildCancel(context.Background(), child.ID(), "second")
	if err != nil {
		t.Errorf("second cancel returned err: %v", err)
	}
	if ok {
		t.Errorf("second cancel ok=true; want false (child already gone)")
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
		wanted := protocol.TerminationUserCancelPrefix + "panic_cancel"
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

// TestRequestChildCancel_CancelFrameEmittedBeforeTerminate — the
// fast-cancel discipline submits Cancel{Cascade:true} BEFORE
// SessionClose so the child's in-flight turn aborts via
// turnCtx.Done and the cascade reaches grandchildren without
// waiting for the mission's current turn to drain. Persisted
// events: Cancel must land in the child's store strictly before
// session_terminated. Phase 5.1c.cancel-ux operator-feedback fix.
func TestRequestChildCancel_CancelEmittedBeforeTerminate(t *testing.T) {
	testStore := fixture.NewTestStore()
	parent, cleanup := newTestParent(t, withTestStore(testStore))
	defer cleanup()

	child, err := parent.Spawn(context.Background(), SpawnSpec{Task: "t"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	if ok, err := parent.RequestChildCancel(context.Background(), child.ID(), "fast"); err != nil || !ok {
		t.Fatalf("RequestChildCancel: ok=%v err=%v", ok, err)
	}
	select {
	case <-child.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("child did not exit within 2s")
	}

	events, _ := testStore.ListEvents(context.Background(), child.ID(), ListEventsOpts{})
	cancelSeq, termSeq := -1, -1
	for i, e := range events {
		switch e.EventType {
		case string(protocol.KindCancel):
			if cancelSeq < 0 {
				cancelSeq = i
			}
		case string(protocol.KindSessionTerminated):
			termSeq = i
		}
	}
	if cancelSeq < 0 {
		t.Fatalf("no cancel event persisted; events=%v", kindsWithReasons(events))
	}
	if termSeq < 0 {
		t.Fatalf("no session_terminated event persisted; events=%v", kindsWithReasons(events))
	}
	if cancelSeq >= termSeq {
		t.Errorf("cancel seq=%d not before terminate seq=%d", cancelSeq, termSeq)
	}
}

// TestStatusFromReason_UserCancelPrefix — the new prefix is mapped
// to its own LLM-visible status enum so the model reading the async
// mission injection sees "user_cancel" instead of falling through to
// "completed". Phase 5.1c.cancel-ux.
func TestStatusFromReason_UserCancelPrefix(t *testing.T) {
	cases := []struct {
		reason string
		want   string
	}{
		{protocol.TerminationUserCancelPrefix + "/mission", "user_cancel"},
		{protocol.TerminationUserCancelPrefix + "panic_cancel", "user_cancel"},
		{protocol.TerminationSubagentCancelPrefix + "x", "subagent_cancel"},
		{protocol.TerminationCompleted, "completed"},
	}
	for _, tc := range cases {
		if got := statusFromReason(tc.reason); got != tc.want {
			t.Errorf("statusFromReason(%q) = %q; want %q", tc.reason, got, tc.want)
		}
	}
}

// TestCloseTurnSkipReason_UserCancelPrefix — the new prefix is
// recognised by the close-turn skip gate so the user-initiated
// cancel does NOT trigger a slow findings-recording turn during
// teardown. Phase 5.1c.cancel-ux.
func TestCloseTurnSkipReason_UserCancelPrefix(t *testing.T) {
	cases := []struct {
		reason string
		skip   bool
	}{
		{protocol.TerminationUserCancelPrefix + "/mission", true},
		{protocol.TerminationUserCancelPrefix + "panic_cancel", true},
		{protocol.TerminationUserCancelPrefix, true},
		{protocol.TerminationSubagentCancelPrefix + "model thought best", false},
		{protocol.TerminationCompleted, false},
	}
	for _, tc := range cases {
		got := closeTurnSkipReason(tc.reason)
		if got != tc.skip {
			t.Errorf("closeTurnSkipReason(%q) = %v; want %v", tc.reason, got, tc.skip)
		}
	}
}
