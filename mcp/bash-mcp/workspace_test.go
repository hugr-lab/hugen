package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// newWS sets up a Workspace and chdirs into a session directory
// so the cwd-anchored "RootSession" branch resolves correctly.
// Caller is responsible for any further fixture setup.
func newWS(t *testing.T) (*Workspace, string) {
	t.Helper()
	root := t.TempDir()
	sessDir := filepath.Join(root, "session")
	shared := filepath.Join(root, "shared")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(sessDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
	w := &Workspace{
		SharedRoot:     shared,
		SharedWritable: true,
	}
	return w, root
}

func TestWorkspace_Resolve_RelativeIntoSession(t *testing.T) {
	w, _ := newWS(t)
	res, err := w.Resolve("file.txt", false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Root != RootSession {
		t.Errorf("Root = %v, want RootSession", res.Root)
	}
}

func TestWorkspace_Resolve_AbsoluteWorkspaceRejected(t *testing.T) {
	// /workspace/* is not in the bash-mcp namespace anymore;
	// the binary lives inside its own writable cwd. Absolute
	// paths under foreign roots are path_escape.
	w, _ := newWS(t)
	_, err := w.Resolve("/workspace/sess-1/data.parquet", false)
	if !errors.Is(err, ErrPathEscape) {
		t.Errorf("err = %v, want ErrPathEscape", err)
	}
}

func TestWorkspace_Resolve_SymlinkEscape(t *testing.T) {
	w, root := newWS(t)
	outside := filepath.Join(root, "outside.txt")
	if err := os.WriteFile(outside, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	cwd, _ := os.Getwd()
	link := filepath.Join(cwd, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	_, err := w.Resolve("escape", false)
	if !errors.Is(err, ErrPathEscape) {
		t.Errorf("err = %v, want ErrPathEscape", err)
	}
}

func TestWorkspace_Resolve_SharedRoundTrip(t *testing.T) {
	w, _ := newWS(t)
	res, err := w.Resolve("/shared/seed.csv", true)
	if err != nil {
		t.Fatalf("Resolve(write=true): %v", err)
	}
	if err := os.WriteFile(res.Canonical, []byte("k,v\na,1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res2, err := w.Resolve("/shared/seed.csv", false)
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

func TestWorkspace_Resolve_SharedDisabled(t *testing.T) {
	w, _ := newWS(t)
	w.SharedRoot = ""
	_, err := w.Resolve("/shared/x", false)
	if !errors.Is(err, ErrSharedDisabled) {
		t.Errorf("err = %v, want ErrSharedDisabled", err)
	}
}

func TestWorkspace_Resolve_SharedReadOnly(t *testing.T) {
	w, _ := newWS(t)
	w.SharedWritable = false
	_, err := w.Resolve("/shared/x", true)
	if !errors.Is(err, ErrReadOnly) {
		t.Errorf("err = %v, want ErrReadOnly", err)
	}
}

func TestWorkspace_Resolve_ReadOnlyEnforcement(t *testing.T) {
	root := t.TempDir()
	roHost := filepath.Join(root, "ro")
	if err := os.MkdirAll(roHost, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roHost, "config.yaml"), []byte("x: 1"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, _ := newWS(t)
	w.ReadonlyMnt = []ReadonlyMnt{{Name: "site", Host: roHost}}
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
	root := t.TempDir()
	w := &Workspace{
		ReadonlyMnt: []ReadonlyMnt{{Name: "missing", Host: filepath.Join(root, "does-not-exist")}},
	}
	err := w.Validate()
	if !errors.Is(err, ErrReadonlyMountMissing) {
		t.Errorf("err = %v, want ErrReadonlyMountMissing", err)
	}
}

func TestWorkspace_Resolve_UnknownReadonlyMountRejected(t *testing.T) {
	w, _ := newWS(t)
	_, err := w.Resolve("/readonly/unknown/file", false)
	if !errors.Is(err, ErrPathEscape) {
		t.Errorf("err = %v, want ErrPathEscape", err)
	}
}

func TestWorkspace_Resolve_RelativeEscapeRejected(t *testing.T) {
	// `../foo` falls outside cwd → path_escape.
	w, _ := newWS(t)
	_, err := w.Resolve("../foo", false)
	if !errors.Is(err, ErrPathEscape) {
		t.Errorf("err = %v, want ErrPathEscape", err)
	}
}
