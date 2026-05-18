package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func newSessionDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	// Simulate the 5.4 mission-shared layout: <workspaces>/<root>/<mission>/
	dir := filepath.Join(root, "workspaces", "ses-root", "ses-mission")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestResolveOutDir_DefaultUnderSessionData(t *testing.T) {
	sessDir := newSessionDir(t)
	got, err := resolveOutDir(sessDir, "", "qid")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(sessDir, "data", "qid")
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	if _, err := os.Stat(got); err != nil {
		t.Fatalf("dir not created: %v", err)
	}
}

func TestResolveOutDir_RelativeAnchorsToSessionRoot(t *testing.T) {
	sessDir := newSessionDir(t)
	got, err := resolveOutDir(sessDir, "results", "qid")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(sessDir, "results")
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	if _, err := os.Stat(got); err != nil {
		t.Fatalf("dir not created: %v", err)
	}
}

func TestResolveOutDir_NestedRelative(t *testing.T) {
	sessDir := newSessionDir(t)
	got, err := resolveOutDir(sessDir, "data/customers/2026", "qid")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(sessDir, "data", "customers", "2026")
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestResolveOutDir_AbsoluteRejected(t *testing.T) {
	sessDir := newSessionDir(t)
	_, err := resolveOutDir(sessDir, "/etc", "qid")
	if err == nil {
		t.Fatal("expected arg_validation error")
	}
	var te *toolError
	if !errors.As(err, &te) || te.Code != "arg_validation" {
		t.Fatalf("err=%v want arg_validation", err)
	}
}

func TestResolveOutDir_DotDotEscapeRejected(t *testing.T) {
	sessDir := newSessionDir(t)
	for _, in := range []string{"../leak", "..", "data/../../leak"} {
		_, err := resolveOutDir(sessDir, in, "qid")
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

func TestResolveOutDir_MissingSessionDir(t *testing.T) {
	_, err := resolveOutDir("", "results", "qid")
	if err == nil {
		t.Fatal("expected error when session_dir is missing")
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
