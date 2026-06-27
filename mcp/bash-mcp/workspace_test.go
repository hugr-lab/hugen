package main

import (
	"errors"
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

// TestWorkspace_Resolve_WriteConfinedToSession verifies that with a
// SessionDir set, write resolution is confined to the workspace: a
// path under it is allowed, a host path or a peer session's dir is
// rejected with ErrHostWriteDenied (F5 — workspace-confined writes
// need no approval).
func TestWorkspace_Resolve_WriteConfinedToSession(t *testing.T) {
	wsRoot := t.TempDir()
	sessDir := filepath.Join(wsRoot, "ses-self")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	w := &Workspace{SessionDir: sessDir, WorkspacesRoot: wsRoot}

	if _, err := w.Resolve(filepath.Join(sessDir, "out.html"), true); err != nil {
		t.Fatalf("write inside workspace: unexpected err %v", err)
	}
	host := t.TempDir() // a separate root, outside wsRoot
	if _, err := w.Resolve(filepath.Join(host, "x.txt"), true); !errors.Is(err, ErrHostWriteDenied) {
		t.Errorf("host write: err = %v, want ErrHostWriteDenied", err)
	}
	peer := filepath.Join(wsRoot, "ses-peer")
	if err := os.MkdirAll(peer, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Resolve(filepath.Join(peer, "x.txt"), true); !errors.Is(err, ErrHostWriteDenied) {
		t.Errorf("peer write: err = %v, want ErrHostWriteDenied", err)
	}
}

// TestWorkspace_Resolve_ReadsStayOpenToHost verifies the read side of
// the F5 split: with a SessionDir set, reads reach host paths outside
// the workspaces root (operator inputs, $SHARED_DIR, skill bundles)
// but still refuse a peer session's scratch under the shared root.
func TestWorkspace_Resolve_ReadsStayOpenToHost(t *testing.T) {
	wsRoot := t.TempDir()
	sessDir := filepath.Join(wsRoot, "ses-self")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	w := &Workspace{SessionDir: sessDir, WorkspacesRoot: wsRoot}

	host := t.TempDir()
	hostFile := filepath.Join(host, "in.csv")
	if err := os.WriteFile(hostFile, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Resolve(hostFile, false); err != nil {
		t.Errorf("host read: unexpected err %v", err)
	}
	peer := filepath.Join(wsRoot, "ses-peer")
	if err := os.MkdirAll(peer, 0o755); err != nil {
		t.Fatal(err)
	}
	peerFile := filepath.Join(peer, "secret.txt")
	if err := os.WriteFile(peerFile, []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Resolve(peerFile, false); !errors.Is(err, ErrCrossSessionPath) {
		t.Errorf("peer read: err = %v, want ErrCrossSessionPath", err)
	}
}
