package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newDeps(t *testing.T) (*queryDeps, string, string) {
	t.Helper()
	root := t.TempDir()
	wsRoot := filepath.Join(root, "workspaces")
	sharedRoot := filepath.Join(root, "shared")
	if err := os.MkdirAll(wsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sharedRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	return &queryDeps{
		workspace: wsRoot,
		shared:    sharedRoot,
		agentID:   "agentX",
	}, wsRoot, sharedRoot
}

func TestResolveOutPath_DefaultUnderSession(t *testing.T) {
	d, ws, _ := newDeps(t)
	got, err := d.resolveOutPath("sess1", "", "qid", "parquet")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(ws, "sess1", "data", "qid.parquet")
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	if _, err := os.Stat(filepath.Dir(got)); err != nil {
		t.Fatalf("parent not created: %v", err)
	}
}

func TestResolveOutPath_RelativeAnchorsToSessionRoot(t *testing.T) {
	d, ws, _ := newDeps(t)
	got, err := d.resolveOutPath("sess1", "out.parquet", "qid", "parquet")
	if err != nil {
		t.Fatal(err)
	}
	// Bare relative anchors at the session root, NOT under data/.
	// The default-path branch (no `path`) is what auto-routes to data/.
	want := filepath.Join(ws, "sess1", "out.parquet")
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestResolveOutPath_RelativeWithDataPrefix(t *testing.T) {
	d, ws, _ := newDeps(t)
	got, err := d.resolveOutPath("sess1", "data/types.json", "qid", "json")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(ws, "sess1", "data", "types.json")
	if got != want {
		t.Fatalf("got %s want %s (no double data/ prefix)", got, want)
	}
}

func TestResolveOutPath_AbsoluteSessionAccepted(t *testing.T) {
	d, ws, _ := newDeps(t)
	abs := filepath.Join(ws, "sess1", "report.json")
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := d.resolveOutPath("sess1", abs, "qid", "json")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got != abs {
		t.Fatalf("got %s want %s", got, abs)
	}
}

func TestResolveOutPath_AbsoluteSharedAccepted(t *testing.T) {
	d, _, sh := newDeps(t)
	abs := filepath.Join(sh, "agentX", "report.json")
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := d.resolveOutPath("sess1", abs, "qid", "json")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !strings.HasPrefix(got, sh) {
		t.Fatalf("got %s want under %s", got, sh)
	}
}

func TestResolveOutPath_OutsideRootsRejected(t *testing.T) {
	d, _, _ := newDeps(t)
	tmp := t.TempDir() // unrelated to workspace
	abs := filepath.Join(tmp, "leak.json")
	_, err := d.resolveOutPath("sess1", abs, "qid", "json")
	if err == nil {
		t.Fatal("expected error")
	}
	var te *toolError
	if !errors.As(err, &te) || te.Code != "path_escape" {
		t.Fatalf("err=%v want path_escape toolError", err)
	}
}

func TestResolveOutPath_PeerSessionRejected(t *testing.T) {
	d, ws, _ := newDeps(t)
	abs := filepath.Join(ws, "other-session", "leak.json")
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := d.resolveOutPath("sess1", abs, "qid", "json")
	if err == nil {
		t.Fatal("expected error")
	}
	var te *toolError
	if !errors.As(err, &te) || te.Code != "path_escape" {
		t.Fatalf("err=%v want path_escape", err)
	}
}

func TestResolveOutPath_SymlinkEscapeBlocked(t *testing.T) {
	d, ws, _ := newDeps(t)
	if err := os.MkdirAll(filepath.Join(ws, "sess1", "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(ws, "sess1", "data", "leak.json")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	_, err := d.resolveOutPath("sess1", link, "qid", "json")
	if err == nil {
		t.Fatal("expected error via canonical-out-of-root")
	}
	var te *toolError
	if !errors.As(err, &te) || te.Code != "path_escape" {
		t.Fatalf("err=%v want path_escape", err)
	}
}

func TestResolveOutPath_MissingSessionID(t *testing.T) {
	d, _, _ := newDeps(t)
	_, err := d.resolveOutPath("", "out.json", "qid", "json")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestNewShortID_NonEmptyAndDifferent(t *testing.T) {
	a, b := newShortID(), newShortID()
	if a == "" || b == "" {
		t.Fatal("empty short id")
	}
	if a == b {
		t.Fatalf("collision %q == %q", a, b)
	}
}

func TestWriteFileAtomic_WritesAndReplaces(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "x", "y", "out.json")
	if err := writeFileAtomic(target, []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "v1" {
		t.Fatalf("v1: got %q", string(b))
	}
	if err := writeFileAtomic(target, []byte("v2")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "v2" {
		t.Fatalf("v2: got %q", string(b))
	}
}

func TestHasDirPrefix(t *testing.T) {
	if !hasDirPrefix("/a/b/c", "/a/b") {
		t.Fatal("/a/b is prefix of /a/b/c")
	}
	if hasDirPrefix("/a/bb", "/a/b") {
		t.Fatal("/a/b must not match /a/bb")
	}
	if !hasDirPrefix("/a/b", "/a/b/") {
		t.Fatal("trailing slash on root must still match self")
	}
	if hasDirPrefix("/x", "") {
		t.Fatal("empty root should not match anything")
	}
}
