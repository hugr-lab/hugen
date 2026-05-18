package workspace

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/internal/fixture"
)

func TestTracker_AcquireAtIdempotent(t *testing.T) {
	root := t.TempDir()
	tr := newTracker(root)

	dir, err := tr.acquireAt("s1", "s1")
	if err != nil {
		t.Fatalf("acquireAt: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("session dir not created: %v", err)
	}
	abs, _ := filepath.Abs(filepath.Join(root, "s1"))
	if dir != abs {
		t.Errorf("dir = %q, want %q", dir, abs)
	}

	// Second call returns the same dir without re-mkdir.
	dir2, err := tr.acquireAt("s1", "s1/something/else")
	if err != nil || dir2 != dir {
		t.Fatalf("acquireAt idempotency broken: dir2=%q err=%v", dir2, err)
	}
}

func TestTracker_AcquireAtNestedPath(t *testing.T) {
	root := t.TempDir()
	tr := newTracker(root)

	dir, err := tr.acquireAt("mission-1", filepath.Join("root-1", "mission-1"))
	if err != nil {
		t.Fatalf("acquireAt: %v", err)
	}
	want, _ := filepath.Abs(filepath.Join(root, "root-1", "mission-1"))
	if dir != want {
		t.Errorf("dir = %q, want %q", dir, want)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("nested dir not created: %v", err)
	}
}

func TestTracker_ForgetIsFilesystemNoop(t *testing.T) {
	root := t.TempDir()
	tr := newTracker(root)
	dir, _ := tr.acquireAt("s1", "s1")
	if got := tr.forget("s1"); got != dir {
		t.Errorf("forget returned %q, want %q", got, dir)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("forget removed dir: %v", err)
	}
	// Forget on unknown id is silent.
	if got := tr.forget("nope"); got != "" {
		t.Errorf("forget(unknown) = %q, want \"\"", got)
	}
}

func TestTracker_SweepOrphans_MissionOnly(t *testing.T) {
	root := t.TempDir()
	tr := newTracker(root)

	// Two roots, each with one live + one stale mission. Roots
	// themselves must survive the sweep.
	mustMkdir := func(parts ...string) string {
		p := filepath.Join(append([]string{root}, parts...)...)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}

	rootA := mustMkdir("root-A")
	liveA := mustMkdir("root-A", "mission-live-A")
	staleA := mustMkdir("root-A", "mission-stale-A")
	rootB := mustMkdir("root-B")
	staleB := mustMkdir("root-B", "mission-stale-B")

	old := time.Now().Add(-48 * time.Hour)
	_ = os.Chtimes(staleA, old, old)
	_ = os.Chtimes(staleB, old, old)
	// rootA mtime old too — must NOT cause root deletion.
	_ = os.Chtimes(rootA, old, old)

	live := map[string]struct{}{"mission-live-A": {}}
	removed, err := tr.sweepOrphans(live, time.Hour)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2", removed)
	}
	if _, err := os.Stat(liveA); err != nil {
		t.Errorf("live mission removed: %v", err)
	}
	if _, err := os.Stat(staleA); !os.IsNotExist(err) {
		t.Errorf("stale mission-A still present")
	}
	if _, err := os.Stat(staleB); !os.IsNotExist(err) {
		t.Errorf("stale mission-B still present")
	}
	if _, err := os.Stat(rootA); err != nil {
		t.Errorf("root-A removed (must persist): %v", err)
	}
	if _, err := os.Stat(rootB); err != nil {
		t.Errorf("root-B removed (must persist): %v", err)
	}
}

func TestTracker_SweepOrphans_DisabledByTTL(t *testing.T) {
	tr := newTracker(t.TempDir())
	removed, err := tr.sweepOrphans(nil, 0)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0 (TTL=0 disables sweep)", removed)
	}
}

func TestExtension_RootTier_PersistsAfterClose(t *testing.T) {
	root := t.TempDir()
	ext := NewExtension(root, nil)
	state := fixture.NewTestSessionState("ses-root")

	ctx := context.Background()
	if err := ext.InitState(ctx, state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	h := FromState(state)
	if h == nil {
		t.Fatalf("state handle missing after InitState")
	}
	if h.Tier() != TierRoot {
		t.Errorf("tier = %v, want TierRoot", h.Tier())
	}
	want, _ := filepath.Abs(filepath.Join(root, "ses-root"))
	if h.Dir() != want {
		t.Errorf("dir = %q, want %q", h.Dir(), want)
	}
	if _, err := os.Stat(h.Dir()); err != nil {
		t.Errorf("session dir not created: %v", err)
	}

	if err := ext.CloseSession(ctx, state); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	// Phase-5.4 contract: dir SURVIVES close at every tier.
	if _, err := os.Stat(h.Dir()); err != nil {
		t.Errorf("root dir removed by CloseSession (5.4 expects persistence): %v", err)
	}
}

func TestExtension_MissionTier_NestedUnderRoot(t *testing.T) {
	root := t.TempDir()
	ext := NewExtension(root, nil)
	rootState := fixture.NewTestSessionState("ses-root")
	mission := fixture.NewTestSessionState("ses-mission").WithParent(rootState)

	ctx := context.Background()
	if err := ext.InitState(ctx, rootState); err != nil {
		t.Fatalf("InitState root: %v", err)
	}
	if err := ext.InitState(ctx, mission); err != nil {
		t.Fatalf("InitState mission: %v", err)
	}
	h := FromState(mission)
	if h == nil {
		t.Fatalf("mission state handle missing")
	}
	if h.Tier() != TierMission {
		t.Errorf("tier = %v, want TierMission", h.Tier())
	}
	want, _ := filepath.Abs(filepath.Join(root, "ses-root", "ses-mission"))
	if h.Dir() != want {
		t.Errorf("mission dir = %q, want %q", h.Dir(), want)
	}
	if _, err := os.Stat(h.Dir()); err != nil {
		t.Errorf("mission dir not created: %v", err)
	}
	// Closing the mission must not delete the dir.
	if err := ext.CloseSession(ctx, mission); err != nil {
		t.Fatalf("CloseSession mission: %v", err)
	}
	if _, err := os.Stat(h.Dir()); err != nil {
		t.Errorf("mission dir removed by close (5.4 expects persistence): %v", err)
	}
}

func TestExtension_MissionClose_LogsPathAtInfo(t *testing.T) {
	root := t.TempDir()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ext := NewExtension(root, logger)

	rootState := fixture.NewTestSessionState("ses-root")
	mission := fixture.NewTestSessionState("ses-mission").WithParent(rootState)
	ctx := context.Background()
	if err := ext.InitState(ctx, rootState); err != nil {
		t.Fatalf("InitState root: %v", err)
	}
	if err := ext.InitState(ctx, mission); err != nil {
		t.Fatalf("InitState mission: %v", err)
	}
	missionDir := FromState(mission).Dir()

	// Closing root must NOT log (would spam operators with chat-id paths
	// that are trivially recoverable from the session id).
	buf.Reset()
	if err := ext.CloseSession(ctx, rootState); err != nil {
		t.Fatalf("CloseSession root: %v", err)
	}
	if strings.Contains(buf.String(), "workspace: mission session closed") {
		t.Errorf("root close emitted mission-close log: %s", buf.String())
	}

	// Closing the mission must emit the INFO line with the resolved dir.
	buf.Reset()
	if err := ext.CloseSession(ctx, mission); err != nil {
		t.Fatalf("CloseSession mission: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "workspace: mission session closed") {
		t.Errorf("mission close did not log expected msg: %s", got)
	}
	if !strings.Contains(got, missionDir) {
		t.Errorf("mission close log missing dir path %q: %s", missionDir, got)
	}
}

func TestExtension_WorkerTier_InheritsMissionDir(t *testing.T) {
	root := t.TempDir()
	ext := NewExtension(root, nil)
	rootState := fixture.NewTestSessionState("ses-root")
	mission := fixture.NewTestSessionState("ses-mission").WithParent(rootState)
	worker1 := fixture.NewTestSessionState("ses-worker-1").WithParent(mission)
	worker2 := fixture.NewTestSessionState("ses-worker-2").WithParent(mission)
	// Deeper grandchild — exercises walkUpToDepth past depth 2.
	grand := fixture.NewTestSessionState("ses-grand").WithParent(worker1)

	ctx := context.Background()
	for _, s := range []*fixture.TestSessionState{rootState, mission, worker1, worker2, grand} {
		if err := ext.InitState(ctx, s); err != nil {
			t.Fatalf("InitState %s: %v", s.SessionID(), err)
		}
	}
	mh := FromState(mission)
	w1 := FromState(worker1)
	w2 := FromState(worker2)
	gh := FromState(grand)
	if w1 == nil || w2 == nil || gh == nil {
		t.Fatalf("worker handles missing")
	}
	if w1.Dir() != mh.Dir() || w2.Dir() != mh.Dir() {
		t.Errorf("workers do not share mission dir: w1=%q w2=%q mission=%q",
			w1.Dir(), w2.Dir(), mh.Dir())
	}
	if gh.Dir() != mh.Dir() {
		t.Errorf("grandchild dir = %q, want mission dir %q", gh.Dir(), mh.Dir())
	}
	if w1.Tier() != TierWorker || w2.Tier() != TierWorker || gh.Tier() != TierWorker {
		t.Errorf("worker tiers wrong: w1=%v w2=%v grand=%v", w1.Tier(), w2.Tier(), gh.Tier())
	}
	// Worker close: dir survives (still owned by mission).
	if err := ext.CloseSession(ctx, worker1); err != nil {
		t.Fatalf("CloseSession worker1: %v", err)
	}
	if _, err := os.Stat(mh.Dir()); err != nil {
		t.Errorf("mission dir gone after worker close: %v", err)
	}
}
