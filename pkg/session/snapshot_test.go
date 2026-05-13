package session

import (
	"context"
	"testing"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// TestSession_Snapshot_BareFreshSession covers the cheap path: a
// freshly-opened root with no children, no extensions writing
// state, no plan / notepad / whiteboard active. Verifies only
// the always-on fields (SessionID, State, Depth, OpenedAt)
// populate; everything else stays zero-valued.
func TestSession_Snapshot_BareFreshSession(t *testing.T) {
	s, cleanup := newTestParent(t)
	defer cleanup()

	snap, err := s.Snapshot(context.Background(), SnapshotOptions{})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.SessionID != s.ID() {
		t.Errorf("SessionID = %q, want %q", snap.SessionID, s.ID())
	}
	if snap.Depth != 0 {
		t.Errorf("Depth = %d, want 0", snap.Depth)
	}
	if snap.OpenedAt.IsZero() {
		t.Errorf("OpenedAt zero — fixture didn't fill openedAt?")
	}
	// No turn running on a fixture session.
	if snap.TurnsUsed != 0 {
		t.Errorf("TurnsUsed = %d, want 0", snap.TurnsUsed)
	}
	if snap.PendingInquiry != nil {
		t.Errorf("PendingInquiry = %+v, want nil", snap.PendingInquiry)
	}
	if snap.LastToolCall != nil {
		t.Errorf("LastToolCall = %+v, want nil", snap.LastToolCall)
	}
}

// TestSession_Snapshot_WithPendingInquiry covers the inquiry
// projection path: a recordPending entry surfaces through
// Snapshot just as it does through populateStatusSnapshot.
func TestSession_Snapshot_WithPendingInquiry(t *testing.T) {
	s, cleanup := newTestParent(t)
	defer cleanup()

	ref := &protocol.PendingInquiryRef{
		RequestID: "req-snap-test",
		Type:      protocol.InquiryTypeApproval,
		Question:  "Run rm -rf /tmp/*?",
	}
	_ = s.recordPending("req-snap-test", ref)
	defer s.clearPending("req-snap-test")

	snap, err := s.Snapshot(context.Background(), SnapshotOptions{})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.PendingInquiry == nil {
		t.Fatal("PendingInquiry nil after recordPending")
	}
	if snap.PendingInquiry.RequestID != "req-snap-test" {
		t.Errorf("RequestID = %q", snap.PendingInquiry.RequestID)
	}
}

// TestSession_Snapshot_WithLastToolCall covers the lastToolCall
// projection: the atomic.Pointer flows through the snapshot,
// returned as a defensive copy.
func TestSession_Snapshot_WithLastToolCall(t *testing.T) {
	s, cleanup := newTestParent(t)
	defer cleanup()

	s.lastToolCall.Store(&protocol.ToolCallRef{
		Name: "bash-mcp:bash.shell",
	})

	snap, err := s.Snapshot(context.Background(), SnapshotOptions{})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.LastToolCall == nil {
		t.Fatal("LastToolCall nil after Store")
	}
	if snap.LastToolCall.Name != "bash-mcp:bash.shell" {
		t.Errorf("LastToolCall.Name = %q", snap.LastToolCall.Name)
	}
}

// TestSession_FindDescendant_SelfMatch covers the trivial case:
// FindDescendant on a session's own id returns the session.
func TestSession_FindDescendant_SelfMatch(t *testing.T) {
	s, cleanup := newTestParent(t)
	defer cleanup()

	got := s.FindDescendant(s.ID())
	if got != s {
		t.Errorf("FindDescendant(self) = %v, want %v", got, s)
	}
}

// TestSession_FindDescendant_NoMatch covers the empty-tree case:
// FindDescendant on an unknown id returns nil.
func TestSession_FindDescendant_NoMatch(t *testing.T) {
	s, cleanup := newTestParent(t)
	defer cleanup()

	if got := s.FindDescendant("ses-not-spawned"); got != nil {
		t.Errorf("FindDescendant(unknown) = %v, want nil", got)
	}
}
