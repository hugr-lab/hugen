package session

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// TestNotifyChild_AddressByName verifies the adapter-callable
// NotifyChild method resolves the target by short name and delivers
// a parent-note frame. Phase 5.2 ε.
func TestNotifyChild_AddressByName(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	_, err := parent.callSpawnSubagent(us1WithSession(parent),
		json.RawMessage(`{"subagents":[{"name":"fetch-q4","task":"go"}]}`))
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	resolvedID, delivered, err := parent.NotifyChild(ctx, "fetch-q4", "narrow to B2B")
	if err != nil {
		t.Fatalf("NotifyChild by name: %v", err)
	}
	if !delivered {
		t.Errorf("NotifyChild by name: delivered=false")
	}
	if resolvedID == "" {
		t.Errorf("NotifyChild by name: resolvedID is empty")
	}
}

// TestNotifyChild_AddressBySessionID verifies the legacy session_id
// addressing form still works after the cherry-picked α.1b
// resolver.
func TestNotifyChild_AddressBySessionID(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	_, err := parent.callSpawnSubagent(us1WithSession(parent),
		json.RawMessage(`{"subagents":[{"name":"fetch","task":"go"}]}`))
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	parent.childMu.Lock()
	var sid string
	for id := range parent.children {
		sid = id
	}
	parent.childMu.Unlock()
	if sid == "" {
		t.Fatalf("could not capture session_id after spawn")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	resolvedID, delivered, err := parent.NotifyChild(ctx, sid, "ping")
	if err != nil {
		t.Fatalf("NotifyChild by session_id: %v", err)
	}
	if !delivered {
		t.Errorf("NotifyChild by session_id: delivered=false")
	}
	if resolvedID != sid {
		t.Errorf("resolvedID = %q, want %q", resolvedID, sid)
	}
}

// TestNotifyChild_NotAChild verifies the "no live child with this
// id" case returns (delivered=false, err=nil) so the slash handler
// can surface a clean no_such_mission error.
func TestNotifyChild_NotAChild(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	resolvedID, delivered, err := parent.NotifyChild(ctx, "nope", "msg")
	if err != nil {
		t.Fatalf("NotifyChild: unexpected err: %v", err)
	}
	if delivered {
		t.Errorf("NotifyChild: delivered=true against unknown target")
	}
	if resolvedID != "" {
		t.Errorf("resolvedID = %q, want empty", resolvedID)
	}
}

// TestNotifyChild_BadArgs covers the empty-target / empty-content
// guards.
func TestNotifyChild_BadArgs(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, _, err := parent.NotifyChild(ctx, "", "msg")
	if !errors.Is(err, ErrNotifyEmptyTarget) {
		t.Errorf("empty target: want ErrNotifyEmptyTarget, got %v", err)
	}

	_, _, err = parent.NotifyChild(ctx, "fetch", "")
	if !errors.Is(err, ErrNotifyEmptyContent) {
		t.Errorf("empty content: want ErrNotifyEmptyContent, got %v", err)
	}

	_, _, err = parent.NotifyChild(ctx, "fetch", "   ")
	if !errors.Is(err, ErrNotifyEmptyContent) {
		t.Errorf("whitespace-only content: want ErrNotifyEmptyContent, got %v", err)
	}
}

// TestSnapshotChildren_NameSort verifies SnapshotChildren returns
// live children sorted by Name with session_id tiebreaker.
func TestSnapshotChildren_NameSort(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	_, err := parent.callSpawnSubagent(us1WithSession(parent),
		json.RawMessage(`{"subagents":[{"name":"zeta","task":"go"},{"name":"alpha","task":"go"},{"name":"mu","task":"go"}]}`))
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	snaps := parent.SnapshotChildren()
	if len(snaps) != 3 {
		t.Fatalf("SnapshotChildren len = %d, want 3", len(snaps))
	}
	want := []string{"alpha", "mu", "zeta"}
	for i, n := range want {
		if snaps[i].Name != n {
			t.Errorf("snaps[%d].Name = %q, want %q (full=%+v)", i, snaps[i].Name, n, snaps)
		}
	}
}
