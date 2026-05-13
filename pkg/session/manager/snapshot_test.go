package manager

import (
	"context"
	"errors"
	"testing"

	"github.com/hugr-lab/hugen/pkg/internal/fixture"
	"github.com/hugr-lab/hugen/pkg/session"
)

// TestSnapshotSession_LiveRoot covers the happy path: a live root
// looked up by id returns a SessionSnapshot whose SessionID +
// Depth match.
func TestSnapshotSession_LiveRoot(t *testing.T) {
	testStore := fixture.NewTestStore()
	mgr := newTestManager(t, testStore)
	ctx := context.Background()
	defer mgr.Stop(ctx)

	root, _, err := mgr.Open(ctx, session.OpenRequest{OwnerID: "alice"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	drainOutboxOnce(root.Outbox())

	snap, err := mgr.SnapshotSession(ctx, root.ID(), session.SnapshotOptions{})
	if err != nil {
		t.Fatalf("SnapshotSession: %v", err)
	}
	if snap.SessionID != root.ID() {
		t.Errorf("SessionID = %q, want %q", snap.SessionID, root.ID())
	}
	if snap.Depth != 0 {
		t.Errorf("Depth = %d, want 0", snap.Depth)
	}
	if snap.Skill != "" || snap.Role != "" {
		t.Errorf("root has spawn metadata? skill=%q role=%q", snap.Skill, snap.Role)
	}
}

// TestSnapshotSession_DescendantWalk covers the descendant lookup:
// SnapshotSession of a subagent id walks the live roots' children
// trees and returns the matching snapshot.
func TestSnapshotSession_DescendantWalk(t *testing.T) {
	testStore := fixture.NewTestStore()
	mgr := newTestManager(t, testStore)
	ctx := context.Background()
	defer mgr.Stop(ctx)

	root, _, err := mgr.Open(ctx, session.OpenRequest{OwnerID: "alice"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	drainOutboxOnce(root.Outbox())

	child, err := root.Spawn(ctx, session.SpawnSpec{Role: "explorer", Task: "t"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	drainOutboxOnce(root.Outbox())
	drainOutboxOnce(child.Outbox())

	snap, err := mgr.SnapshotSession(ctx, child.ID(), session.SnapshotOptions{})
	if err != nil {
		t.Fatalf("SnapshotSession(child): %v", err)
	}
	if snap.SessionID != child.ID() {
		t.Errorf("SessionID = %q, want %q", snap.SessionID, child.ID())
	}
	if snap.Depth != 1 {
		t.Errorf("Depth = %d, want 1", snap.Depth)
	}
	if snap.Role != "explorer" {
		t.Errorf("Role = %q, want explorer", snap.Role)
	}
}

// TestSnapshotSession_NotFound covers the lookup-miss path: an id
// that is neither a live root nor a descendant returns
// ErrSnapshotSessionNotFound.
func TestSnapshotSession_NotFound(t *testing.T) {
	testStore := fixture.NewTestStore()
	mgr := newTestManager(t, testStore)
	ctx := context.Background()
	defer mgr.Stop(ctx)

	_, err := mgr.SnapshotSession(ctx, "ses-does-not-exist", session.SnapshotOptions{})
	if !errors.Is(err, session.ErrSnapshotSessionNotFound) {
		t.Errorf("err = %v, want ErrSnapshotSessionNotFound", err)
	}
}
