package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWorkspace_AcquireReleaseCleanup(t *testing.T) {
	root := t.TempDir()
	ws := NewWorkspace(root, true)

	dir, err := ws.Acquire("s1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dir missing: %v", err)
	}
	wantPrefix, _ := filepath.Abs(root)
	if filepath.Dir(dir) != wantPrefix {
		t.Fatalf("dir %q not under root %q", dir, wantPrefix)
	}

	dir2, err := ws.Acquire("s1")
	if err != nil || dir2 != dir {
		t.Fatalf("Acquire idempotency broken: dir2=%q err=%v", dir2, err)
	}

	got, err := ws.Release("s1")
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got != dir {
		t.Fatalf("Release dir = %q, want %q", got, dir)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("cleanup did not remove dir, stat err = %v", err)
	}
}

func TestWorkspace_ReleaseNoCleanup(t *testing.T) {
	root := t.TempDir()
	ws := NewWorkspace(root, false)
	dir, _ := ws.Acquire("s1")

	if _, err := ws.Release("s1"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dir was removed when cleanup=false: %v", err)
	}
}

func TestWorkspace_ReleaseUnknown(t *testing.T) {
	ws := NewWorkspace(t.TempDir(), true)
	dir, err := ws.Release("never-acquired")
	if err != nil {
		t.Fatalf("Release unknown: %v", err)
	}
	if dir != "" {
		t.Fatalf("Release unknown returned %q, want empty", dir)
	}
}

func TestWorkspace_SweepOrphans(t *testing.T) {
	root := t.TempDir()
	ws := NewWorkspace(root, true)
	live := filepath.Join(root, "ses-live")
	orphan := filepath.Join(root, "ses-old")
	fresh := filepath.Join(root, "ses-fresh")
	for _, p := range []string{live, orphan, fresh} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * time.Hour)
	_ = os.Chtimes(orphan, old, old)

	removed, err := ws.SweepOrphans(map[string]struct{}{"ses-live": {}}, time.Hour)
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed=%d want 1", removed)
	}
	if _, err := os.Stat(live); err != nil {
		t.Errorf("live session removed: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("orphan still on disk")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh session removed: %v", err)
	}
}

func TestWorkspace_SweepOrphans_DisabledByTTL(t *testing.T) {
	root := t.TempDir()
	ws := NewWorkspace(root, true)
	if err := os.MkdirAll(filepath.Join(root, "any"), 0o755); err != nil {
		t.Fatal(err)
	}
	removed, err := ws.SweepOrphans(nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("ttl=0 should disable sweep, removed=%d", removed)
	}
	if _, err := os.Stat(filepath.Join(root, "any")); err != nil {
		t.Errorf("disabled sweep removed dir: %v", err)
	}
}
