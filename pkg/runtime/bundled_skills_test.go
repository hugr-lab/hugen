package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallBundledSkills_FreshInstall(t *testing.T) {
	state := t.TempDir()
	if err := InstallBundledSkills(state, discardLogger()); err != nil {
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
	if err := InstallBundledSkills(state, discardLogger()); err != nil {
		t.Fatalf("first install: %v", err)
	}
	manifest := filepath.Join(state, "skills/system/_system/SKILL.md")
	first, err := os.Stat(manifest)
	if err != nil {
		t.Fatalf("first stat: %v", err)
	}

	if err := InstallBundledSkills(state, discardLogger()); err != nil {
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
	if err := InstallBundledSkills(state, discardLogger()); err != nil {
		t.Fatalf("install: %v", err)
	}
	checksumPath := filepath.Join(state, "skills/system/_system/.hugen-checksum")
	if err := os.WriteFile(checksumPath, []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(state, "skills/system/_system/leftover.txt")
	if err := os.WriteFile(stray, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := InstallBundledSkills(state, discardLogger()); err != nil {
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

func TestInstallBundledSkills_EmptyStateDir(t *testing.T) {
	if err := InstallBundledSkills("", discardLogger()); err == nil {
		t.Fatal("expected error for empty state dir")
	}
}

func TestPhaseBundledSkills_Direct(t *testing.T) {
	state := t.TempDir()
	core := &Core{
		Cfg:    Config{StateDir: state},
		Logger: discardLogger(),
	}
	if err := phaseBundledSkills(core); err != nil {
		t.Fatalf("phase: %v", err)
	}
	manifest := filepath.Join(state, "skills/system/_system/SKILL.md")
	if _, err := os.Stat(manifest); err != nil {
		t.Errorf("phase did not install bundled skill: %v", err)
	}
}
