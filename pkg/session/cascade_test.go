package session

import (
	"context"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/internal/fixture"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

// TestTerminate_CascadeWritesCancelCascade asserts that when a parent
// is explicitly terminated, the subagent's session_terminated event
// records reason="cancel_cascade" — not the parent's reason — and no
// SessionClosed Frame is emitted on the subagent's transcript.
func TestTerminate_CascadeWritesCancelCascade(t *testing.T) {
	testStore := fixture.NewTestStore()
	mgr := newTestManager(t, testStore)
	ctx := context.Background()
	defer mgr.Stop(ctx)

	parent, _, err := mgr.Open(ctx, OpenRequest{OwnerID: "alice"})
	if err != nil {
		t.Fatalf("open parent: %v", err)
	}
	drainOutboxOnce(parent.Outbox())

	child, err := parent.Spawn(ctx, SpawnSpec{Role: "explorer", Task: "t"})
	if err != nil {
		t.Fatalf("spawn child: %v", err)
	}
	drainOutboxOnce(parent.Outbox()) // subagent_started on parent
	drainOutboxOnce(child.Outbox())

	// Terminate the parent. Cascade should fire on the child.
	if err := mgr.Terminate(ctx, parent.ID(), "user:/end manual"); err != nil {
		t.Fatalf("terminate parent: %v", err)
	}

	// Wait for the child's goroutine to exit so its session_terminated
	// event has landed.
	select {
	case <-child.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("child goroutine did not exit within 2s of parent terminate")
	}

	childEvents, _ := testStore.ListEvents(ctx, child.ID(), store.ListEventsOpts{})
	if !containsKindWithReason(childEvents, protocol.KindSessionTerminated, protocol.TerminationCancelCascade) {
		t.Errorf("child session_terminated reason ≠ cancel_cascade: events=%v", kindsWithReasons(childEvents))
	}
	for _, ev := range childEvents {
		if ev.EventType == string(protocol.KindSessionClosed) {
			t.Errorf("child unexpectedly emitted SessionClosed on cascade")
		}
	}

	parentEvents, _ := testStore.ListEvents(ctx, parent.ID(), store.ListEventsOpts{})
	if !containsKindWithReason(parentEvents, protocol.KindSessionTerminated, "user:/end manual") {
		t.Errorf("parent session_terminated reason ≠ user:/end manual: events=%v", kindsWithReasons(parentEvents))
	}
}

// TestTerminate_ExplicitWritesCallerReason asserts the non-cascade
// path is unchanged: an explicitly-terminated session writes
// session_terminated with the caller-supplied reason verbatim.
func TestTerminate_ExplicitWritesCallerReason(t *testing.T) {
	testStore := fixture.NewTestStore()
	mgr := newTestManager(t, testStore)
	ctx := context.Background()
	defer mgr.Stop(ctx)

	s, _, err := mgr.Open(ctx, OpenRequest{OwnerID: "alice"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	drainOutboxOnce(s.Outbox())

	if err := mgr.Terminate(ctx, s.ID(), "test_reason"); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	select {
	case <-s.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("session goroutine did not exit within 2s")
	}

	events, _ := testStore.ListEvents(ctx, s.ID(), store.ListEventsOpts{})
	if !containsKindWithReason(events, protocol.KindSessionTerminated, "test_reason") {
		t.Errorf("session_terminated reason ≠ test_reason: events=%v", kindsWithReasons(events))
	}
}

func kindsWithReasons(events []store.EventRow) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		r, _ := ev.Metadata["reason"].(string)
		if r != "" {
			out = append(out, ev.EventType+"{"+r+"}")
		} else {
			out = append(out, ev.EventType)
		}
	}
	return out
}
