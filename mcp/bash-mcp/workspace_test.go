package main

import (
	"os"
	"path/filepath"
	"testing"
)

// chdirTo enters tmpDir for the test and restores the previous cwd
// on cleanup. Workspace.Resolve canonicalises against os.Getwd, so
// every test sits inside its own scratch dir.
func chdirTo(t *testing.T, dir string) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
}

func TestWorkspace_Resolve_RelativeAnchorsToCwd(t *testing.T) {
	root := t.TempDir()
	chdirTo(t, root)
	if err := os.WriteFile("file.txt", []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := &Workspace{}
	res, err := w.Resolve("file.txt", false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want, _ := filepath.EvalSymlinks(filepath.Join(root, "file.txt"))
	if res.Canonical != want {
		t.Errorf("Canonical = %q, want %q", res.Canonical, want)
	}
}

func TestWorkspace_Resolve_AbsolutePassesThrough(t *testing.T) {
	root := t.TempDir()
	abs := filepath.Join(root, "data")
	if err := os.MkdirAll(abs, 0o755); err != nil {
		t.Fatal(err)
	}
	w := &Workspace{}
	res, err := w.Resolve(abs, false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want, _ := filepath.EvalSymlinks(abs)
	if res.Canonical != want {
		t.Errorf("Canonical = %q, want %q", res.Canonical, want)
	}
}

func TestWorkspace_Resolve_NewFileResolvesParent(t *testing.T) {
	// Writes target a file that doesn't exist yet; canonicalise
	// must resolve the parent dir + reattach the basename.
	root := t.TempDir()
	chdirTo(t, root)
	w := &Workspace{}
	res, err := w.Resolve("new-file.txt", true)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := filepath.Join(root, "new-file.txt")
	gotAbs, _ := filepath.Abs(res.Canonical)
	wantAbs, _ := filepath.EvalSymlinks(filepath.Dir(want))
	wantAbs = filepath.Join(wantAbs, filepath.Base(want))
	if gotAbs != wantAbs {
		t.Errorf("Canonical = %q, want %q", gotAbs, wantAbs)
	}
}
