package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/internal/fixture"
)

func TestTracker_AcquireReleaseCleanup(t *testing.T) {
	root := t.TempDir()
	tr := newTracker(root, true)

	dir, err := tr.acquire("s1")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("session dir not created: %v", err)
	}
	abs, err := filepath.Abs(filepath.Join(root, "s1"))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	if dir != abs {
		t.Errorf("dir = %q, want %q", dir, abs)
	}

	dir2, err := tr.acquire("s1")
	if err != nil || dir2 != dir {
		t.Fatalf("acquire idempotency broken: dir2=%q err=%v", dir2, err)
	}

	if err := tr.release("s1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("dir still exists after release: %v", err)
	}
}

func TestTracker_ReleaseNoCleanup(t *testing.T) {
	root := t.TempDir()
	tr := newTracker(root, false)
	dir, _ := tr.acquire("s1")
	if err := tr.release("s1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("cleanup=false but dir was removed: %v", err)
	}
}

func TestTracker_SweepOrphans(t *testing.T) {
	root := t.TempDir()
	tr := newTracker(root, true)
	live := filepath.Join(root, "ses-live")
	if err := os.MkdirAll(live, 0o755); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(root, "ses-old")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	_ = os.Chtimes(orphan, old, old)

	removed, err := tr.sweepOrphans(map[string]struct{}{"ses-live": {}}, time.Hour)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(live); err != nil {
		t.Errorf("live session removed: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("orphan still on disk")
	}
}

func TestTracker_SweepOrphans_DisabledByTTL(t *testing.T) {
	tr := newTracker(t.TempDir(), false)
	removed, err := tr.sweepOrphans(nil, 0)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0 (TTL=0 disables sweep)", removed)
	}
}

func TestExtension_InitStateAcquiresAndCleansUp(t *testing.T) {
	root := t.TempDir()
	ext := NewExtension(root, true)
	state := fixture.NewTestSessionState("ses-ext")

	ctx := context.Background()
	if err := ext.InitState(ctx, state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	h := FromState(state)
	if h == nil {
		t.Fatalf("state handle missing after InitState")
	}
	if h.Dir() == "" || h.Root() == "" {
		t.Errorf("handle paths empty: dir=%q root=%q", h.Dir(), h.Root())
	}
	if _, err := os.Stat(h.Dir()); err != nil {
		t.Errorf("session dir not created: %v", err)
	}

	if err := ext.CloseSession(ctx, state); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	if _, err := os.Stat(h.Dir()); !os.IsNotExist(err) {
		t.Errorf("session dir not removed by CloseSession: %v", err)
	}
}
