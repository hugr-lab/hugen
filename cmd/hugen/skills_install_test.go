package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestInstallBundledSkills_FreshInstall(t *testing.T) {
	state := t.TempDir()
	if err := installBundledSkills(state, newTestLogger()); err != nil {
		t.Fatalf("install: %v", err)
	}
	manifest := filepath.Join(state, "skills/system/_system/SKILL.md")
	body, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if !strings.Contains(string(body), "name: _system") {
		t.Errorf("manifest content unexpected:\n%s", body)
	}
	if _, err := os.Stat(filepath.Join(state, "skills/system/_system/.hugen-checksum")); err != nil {
		t.Errorf("checksum file missing: %v", err)
	}
}

func TestInstallBundledSkills_Idempotent(t *testing.T) {
	state := t.TempDir()
	if err := installBundledSkills(state, newTestLogger()); err != nil {
		t.Fatalf("first install: %v", err)
	}
	manifest := filepath.Join(state, "skills/system/_system/SKILL.md")
	first, err := os.Stat(manifest)
	if err != nil {
		t.Fatalf("first stat: %v", err)
	}

	if err := installBundledSkills(state, newTestLogger()); err != nil {
		t.Fatalf("second install: %v", err)
	}
	second, err := os.Stat(manifest)
	if err != nil {
		t.Fatalf("second stat: %v", err)
	}
	if !first.ModTime().Equal(second.ModTime()) {
		t.Errorf("idempotent re-install rewrote file: %v -> %v", first.ModTime(), second.ModTime())
	}
}

func TestInstallBundledSkills_ChecksumMismatchReplaces(t *testing.T) {
	state := t.TempDir()
	if err := installBundledSkills(state, newTestLogger()); err != nil {
		t.Fatalf("install: %v", err)
	}
	checksumPath := filepath.Join(state, "skills/system/_system/.hugen-checksum")
	if err := os.WriteFile(checksumPath, []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Drop a stray file that survived from a prior install — the
	// installer should wipe the directory before re-materialising.
	stray := filepath.Join(state, "skills/system/_system/leftover.txt")
	if err := os.WriteFile(stray, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := installBundledSkills(state, newTestLogger()); err != nil {
		t.Fatalf("re-install: %v", err)
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Errorf("stray file survived re-install: %v", err)
	}
	body, err := os.ReadFile(checksumPath)
	if err != nil {
		t.Fatalf("read checksum: %v", err)
	}
	if strings.TrimSpace(string(body)) == "stale" {
		t.Errorf("checksum not refreshed: %q", body)
	}
}
