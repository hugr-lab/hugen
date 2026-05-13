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
	if got := LookupLatestStatusEvent(rows); got != "wait_subagents" {
		t.Errorf("got %q, want wait_subagents", got)
	}

	if got := LookupLatestStatusEvent(nil); got != "" {
		t.Errorf("empty events: got %q, want empty", got)
	}

	noStatus := []EventRow{{EventType: string(protocol.KindUserMessage)}}
	if got := LookupLatestStatusEvent(noStatus); got != "" {
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
		Consumes:       func(f protocol.Frame) bool { return false },
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

// TestPopulateStatusSnapshot_EmptySession verifies the phase-5.1b
// enrichment fields stay nil / empty for a fresh session with no
// children, no inquiry, no tool dispatched. Wire format must keep
// the omitempty contract — pre-5.1b adapters MUST see the
// original {state, reason} shape.
func TestPopulateStatusSnapshot_EmptySession(t *testing.T) {
	s, cleanup := newTestParent(t)
	defer cleanup()

	var payload protocol.SessionStatusPayload
	s.populateStatusSnapshot(&payload)
	if payload.ActiveSubagents != nil {
		t.Errorf("ActiveSubagents = %+v, want nil", payload.ActiveSubagents)
	}
	if payload.PendingInquiry != nil {
		t.Errorf("PendingInquiry = %+v, want nil", payload.PendingInquiry)
	}
	if payload.LastToolCall != nil {
		t.Errorf("LastToolCall = %+v, want nil", payload.LastToolCall)
	}
}

// TestPopulateStatusSnapshot_WithPendingInquiry covers the inquiry
// snapshot path: a recordPending call with a non-nil ref must
// surface through populateStatusSnapshot as a coherent
// PendingInquiryRef.
func TestPopulateStatusSnapshot_WithPendingInquiry(t *testing.T) {
	s, cleanup := newTestParent(t)
	defer cleanup()

	ref := &protocol.PendingInquiryRef{
		RequestID: "req-snap-1",
		Type:      protocol.InquiryTypeClarification,
		Question:  "northwind or transport?",
	}
	_ = s.recordPending("req-snap-1", ref)
	defer s.clearPending("req-snap-1")

	var payload protocol.SessionStatusPayload
	s.populateStatusSnapshot(&payload)
	if payload.PendingInquiry == nil {
		t.Fatal("PendingInquiry nil after recordPending")
	}
	if payload.PendingInquiry.RequestID != "req-snap-1" {
		t.Errorf("RequestID = %q, want req-snap-1", payload.PendingInquiry.RequestID)
	}
	if payload.PendingInquiry.Question != "northwind or transport?" {
		t.Errorf("Question = %q", payload.PendingInquiry.Question)
	}
	// Mutating the returned ref MUST NOT affect the live state —
	// the helper returns a clone.
	payload.PendingInquiry.Question = "MUTATED"
	var second protocol.SessionStatusPayload
	s.populateStatusSnapshot(&second)
	if second.PendingInquiry.Question != "northwind or transport?" {
		t.Errorf("snapshot was mutable: %q", second.PendingInquiry.Question)
	}
}

// TestPopulateStatusSnapshot_WithLastToolCall asserts the
// lastToolCall atomic.Pointer flows into the payload, with the
// returned ref a copy so adapters can't mutate live state.
func TestPopulateStatusSnapshot_WithLastToolCall(t *testing.T) {
	s, cleanup := newTestParent(t)
	defer cleanup()

	s.lastToolCall.Store(&protocol.ToolCallRef{
		Name: "hugr-main:discovery-search_data_sources",
	})

	var payload protocol.SessionStatusPayload
	s.populateStatusSnapshot(&payload)
	if payload.LastToolCall == nil {
		t.Fatal("LastToolCall nil after Store")
	}
	if payload.LastToolCall.Name != "hugr-main:discovery-search_data_sources" {
		t.Errorf("LastToolCall.Name = %q", payload.LastToolCall.Name)
	}
	payload.LastToolCall.Name = "MUTATED"
	var second protocol.SessionStatusPayload
	s.populateStatusSnapshot(&second)
	if second.LastToolCall.Name != "hugr-main:discovery-search_data_sources" {
		t.Errorf("snapshot was mutable: %q", second.LastToolCall.Name)
	}
}
