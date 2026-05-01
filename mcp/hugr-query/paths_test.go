package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func newDeps(t *testing.T) (*queryDeps, string) {
	t.Helper()
	root := t.TempDir()
	wsRoot := filepath.Join(root, "workspaces")
	if err := os.MkdirAll(wsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	return &queryDeps{workspace: wsRoot}, wsRoot
}

func TestResolveOutDir_DefaultUnderSessionData(t *testing.T) {
	d, ws := newDeps(t)
	got, err := d.resolveOutDir("sess1", "", "qid")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(ws, "sess1", "data", "qid")
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	if _, err := os.Stat(got); err != nil {
		t.Fatalf("dir not created: %v", err)
	}
}

func TestResolveOutDir_RelativeAnchorsToSessionRoot(t *testing.T) {
	d, ws := newDeps(t)
	got, err := d.resolveOutDir("sess1", "results", "qid")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(ws, "sess1", "results")
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	if _, err := os.Stat(got); err != nil {
		t.Fatalf("dir not created: %v", err)
	}
}

func TestResolveOutDir_NestedRelative(t *testing.T) {
	d, ws := newDeps(t)
	got, err := d.resolveOutDir("sess1", "data/customers/2026", "qid")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(ws, "sess1", "data", "customers", "2026")
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestResolveOutDir_AbsoluteRejected(t *testing.T) {
	d, _ := newDeps(t)
	_, err := d.resolveOutDir("sess1", "/etc", "qid")
	if err == nil {
		t.Fatal("expected arg_validation error")
	}
	var te *toolError
	if !errors.As(err, &te) || te.Code != "arg_validation" {
		t.Fatalf("err=%v want arg_validation", err)
	}
}

func TestResolveOutDir_DotDotEscapeRejected(t *testing.T) {
	d, _ := newDeps(t)
	for _, in := range []string{"../leak", "..", "data/../../leak"} {
		_, err := d.resolveOutDir("sess1", in, "qid")
		if err == nil {
			t.Errorf("%q: expected error", in)
			continue
		}
		var te *toolError
		if !errors.As(err, &te) || te.Code != "arg_validation" {
			t.Errorf("%q: err=%v want arg_validation", in, err)
		}
	}
}

func TestResolveOutDir_MissingSessionID(t *testing.T) {
	d, _ := newDeps(t)
	_, err := d.resolveOutDir("", "results", "qid")
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
