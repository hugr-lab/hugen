package session

import (
	"context"
	"testing"

	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/internal/fixture"
)

// TestMarkStatus_TransitionGuard covers the dedupe contract:
// markStatus only emits a marker when the target state differs
// from the current one. Repeated identical calls are a no-op.
func TestMarkStatus_TransitionGuard(t *testing.T) {
	store := fixture.NewTestStore()
	s, cleanup := newTestParent(t, withTestStore(store))
	defer cleanup()

	ctx := context.Background()

	s.markStatus(ctx, protocol.SessionStatusActive, "first")
	s.markStatus(ctx, protocol.SessionStatusActive, "duplicate")
	s.markStatus(ctx, protocol.SessionStatusIdle, "settled")
	s.markStatus(ctx, protocol.SessionStatusActive, "second")

	if got := s.Status(); got != protocol.SessionStatusActive {
		t.Errorf("Status() = %q, want %q", got, protocol.SessionStatusActive)
	}

	rows, err := store.ListEvents(ctx, s.ID(), ListEventsOpts{})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	statusEvents := 0
	for _, r := range rows {
		if r.EventType == string(protocol.KindSessionStatus) {
			statusEvents++
		}
	}
	// initial idle (newSession) + active + idle + active — four
	// transitions; one duplicate active dropped by the guard.
	if statusEvents != 4 {
		t.Errorf("status events persisted = %d, want 4 (guard drops dupes; initial idle from newSession)", statusEvents)
	}
}

// TestLookupLatestStatusEvent walks a synthetic event slice and
// asserts the helper returns the newest KindSessionStatus row's
// state, ignoring intervening non-status events.
func TestLookupLatestStatusEvent(t *testing.T) {
	rows := []EventRow{
		{EventType: string(protocol.KindSessionStatus), Metadata: map[string]any{"state": "idle"}},
		{EventType: string(protocol.KindUserMessage)},
		{EventType: string(protocol.KindSessionStatus), Metadata: map[string]any{"state": "active"}},
		{EventType: string(protocol.KindAgentMessage)},
		{EventType: string(protocol.KindSessionStatus), Metadata: map[string]any{"state": "wait_subagents"}},
		{EventType: string(protocol.KindToolCall)},
	}
	if got := lookupLatestStatusEvent(rows); got != "wait_subagents" {
		t.Errorf("got %q, want wait_subagents", got)
	}

	if got := lookupLatestStatusEvent(nil); got != "" {
		t.Errorf("empty events: got %q, want empty", got)
	}

	noStatus := []EventRow{{EventType: string(protocol.KindUserMessage)}}
	if got := lookupLatestStatusEvent(noStatus); got != "" {
		t.Errorf("no status rows: got %q, want empty", got)
	}
}

// TestRegisterToolFeed_TransitionsAndReleasesIdempotently asserts
// the helper installs the feed, transitions to BlockingState,
// releases the feed, and transitions back to active. The release
// closure must be idempotent so defer-double-call patterns are safe.
func TestRegisterToolFeed_TransitionsAndReleasesIdempotently(t *testing.T) {
	s, cleanup := newTestParent(t)
	defer cleanup()

	ctx := context.Background()
	s.markStatus(ctx, protocol.SessionStatusActive, "test_setup")

	feed := &ToolFeed{
		Consumes:       func(k protocol.Kind) bool { return false },
		Feed:           func(f protocol.Frame) {},
		BlockingState:  protocol.SessionStatusWaitSubagents,
		BlockingReason: "test=feed",
	}
	release := s.registerToolFeed(ctx, feed)

	if got := s.activeToolFeed.Load(); got != feed {
		t.Errorf("activeToolFeed = %v, want feed", got)
	}
	if got := s.Status(); got != protocol.SessionStatusWaitSubagents {
		t.Errorf("Status() = %q, want wait_subagents", got)
	}

	release()
	if got := s.activeToolFeed.Load(); got != nil {
		t.Errorf("activeToolFeed after release = %v, want nil", got)
	}
	if got := s.Status(); got != protocol.SessionStatusActive {
		t.Errorf("Status() after release = %q, want active", got)
	}

	// Double release is a no-op.
	release()
	if got := s.Status(); got != protocol.SessionStatusActive {
		t.Errorf("Status() after second release = %q, want active", got)
	}
}

// TestIsQuiescent_FreshSession asserts a freshly-opened root with
// no children, no buffered frames, no active feed reports
// quiescent. Smoke probe — full state machine wiring covered
// in the integration suite.
func TestIsQuiescent_FreshSession(t *testing.T) {
	s, cleanup := newTestParent(t)
	defer cleanup()

	if !s.isQuiescent() {
		t.Errorf("fresh session not quiescent: turnState=%v feed=%v pending=%d",
			s.turnState, s.activeToolFeed.Load(), len(s.pendingInbound))
	}
}
