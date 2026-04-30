package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newWS(t *testing.T) (*Workspace, string) {
	t.Helper()
	root := t.TempDir()
	wsRoot := filepath.Join(root, "workspace")
	shared := filepath.Join(root, "shared")
	if err := os.MkdirAll(wsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	w := &Workspace{
		WorkspaceRoot: wsRoot,
		SharedRoot:    shared,
		AgentID:       "agent-1",
		SessionID:     "sess-1",
	}
	if err := w.EnsureSessionDirs(); err != nil {
		t.Fatal(err)
	}
	return w, root
}

func TestWorkspace_Resolve_RelativeIntoSession(t *testing.T) {
	w, _ := newWS(t)
	res, err := w.Resolve("file.txt", false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Root != RootWorkspace {
		t.Errorf("Root = %v, want RootWorkspace", res.Root)
	}
	if !strings.HasSuffix(res.Logical, "/workspace/sess-1/file.txt") {
		t.Errorf("Logical = %s", res.Logical)
	}
}

func TestWorkspace_Resolve_AbsoluteWorkspaceOK(t *testing.T) {
	w, _ := newWS(t)
	res, err := w.Resolve("/workspace/sess-1/data.parquet", false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Root != RootWorkspace {
		t.Errorf("Root = %v", res.Root)
	}
}

func TestWorkspace_Resolve_AbsoluteCrossSessionRejected(t *testing.T) {
	w, _ := newWS(t)
	_, err := w.Resolve("/workspace/sess-other/secret", false)
	if !errors.Is(err, ErrPathEscape) {
		t.Errorf("err = %v, want ErrPathEscape", err)
	}
}

func TestWorkspace_Resolve_SymlinkEscape(t *testing.T) {
	// 3a: file inside the session that symlinks to a host path.
	w, root := newWS(t)
	outside := filepath.Join(root, "outside.txt")
	if err := os.WriteFile(outside, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(w.WorkspaceRoot, w.SessionID, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	_, err := w.Resolve("/workspace/sess-1/escape", false)
	if !errors.Is(err, ErrPathEscape) {
		t.Errorf("err = %v, want ErrPathEscape", err)
	}
}

func TestWorkspace_Resolve_SharedRoundTrip(t *testing.T) {
	// 3b: write via /shared, read back via /shared.
	w, _ := newWS(t)
	res, err := w.Resolve("/shared/agent-1/seed.csv", true)
	if err != nil {
		t.Fatalf("Resolve(write=true): %v", err)
	}
	if err := os.WriteFile(res.Canonical, []byte("k,v\na,1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res2, err := w.Resolve("/shared/agent-1/seed.csv", false)
	if err != nil {
		t.Fatalf("Resolve(read): %v", err)
	}
	body, err := os.ReadFile(res2.Canonical)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "k,v\na,1\n" {
		t.Errorf("readback = %q", body)
	}
}

func TestWorkspace_Resolve_ReadOnlyEnforcement(t *testing.T) {
	// 3c: read OK, write rejected.
	root := t.TempDir()
	roHost := filepath.Join(root, "ro")
	if err := os.MkdirAll(roHost, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roHost, "config.yaml"), []byte("x: 1"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := &Workspace{
		WorkspaceRoot: filepath.Join(root, "workspace"),
		SharedRoot:    filepath.Join(root, "shared"),
		ReadonlyMnt:   []ReadonlyMnt{{Name: "site", Host: roHost}},
		AgentID:       "a1",
		SessionID:     "s1",
	}
	_ = os.MkdirAll(w.WorkspaceRoot, 0o755)
	_ = os.MkdirAll(w.SharedRoot, 0o755)
	_ = w.EnsureSessionDirs()
	if err := w.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	res, err := w.Resolve("/readonly/site/config.yaml", false)
	if err != nil {
		t.Fatalf("Resolve(read): %v", err)
	}
	if res.Root != RootReadOnly {
		t.Errorf("Root = %v", res.Root)
	}
	_, err = w.Resolve("/readonly/site/config.yaml", true)
	if !errors.Is(err, ErrReadOnly) {
		t.Errorf("err = %v, want ErrReadOnly", err)
	}
}

func TestWorkspace_Validate_MissingMountFailsBoot(t *testing.T) {
	// 3c failure path.
	root := t.TempDir()
	w := &Workspace{
		WorkspaceRoot: filepath.Join(root, "workspace"),
		SharedRoot:    filepath.Join(root, "shared"),
		ReadonlyMnt:   []ReadonlyMnt{{Name: "missing", Host: filepath.Join(root, "does-not-exist")}},
		AgentID:       "a1",
		SessionID:     "s1",
	}
	err := w.Validate()
	if !errors.Is(err, ErrReadonlyMountMissing) {
		t.Errorf("err = %v, want ErrReadonlyMountMissing", err)
	}
}

func TestWorkspace_SweepOrphans(t *testing.T) {
	// 3d: orphan TTL cleanup.
	w, _ := newWS(t)
	// Create an orphan workspace dir well in the past.
	orphan := filepath.Join(w.WorkspaceRoot, "sess-old")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}
	w.OrphanTTL = time.Hour
	removed, err := w.SweepOrphans()
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("orphan still exists: %v", err)
	}
}

func TestWorkspace_SweepOrphans_PreservesLiveSession(t *testing.T) {
	w, _ := newWS(t)
	w.OrphanTTL = time.Nanosecond // every dir looks expired
	live := filepath.Join(w.WorkspaceRoot, w.SessionID)
	if _, err := os.Stat(live); err != nil {
		t.Fatal(err)
	}
	if _, err := w.SweepOrphans(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Errorf("live session removed: %v", err)
	}
}

func TestWorkspace_Resolve_UnknownReadonlyMountRejected(t *testing.T) {
	w, _ := newWS(t)
	_, err := w.Resolve("/readonly/unknown/file", false)
	if !errors.Is(err, ErrPathEscape) {
		t.Errorf("err = %v, want ErrPathEscape", err)
	}
}
