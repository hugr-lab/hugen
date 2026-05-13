package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hubTestSkill is a hub-tier skill we know ships with the binary
// — used as a stable witness for install/refresh tests. Switch
// to another bundled name if `hugr-data` is ever retired.
const hubTestSkill = "hugr-data"

func TestInstallBundledHubSkills_FreshInstall(t *testing.T) {
	state := t.TempDir()
	if err := InstallBundledHubSkills(state, discardLogger()); err != nil {
		t.Fatalf("install: %v", err)
	}
	manifest := filepath.Join(state, "skills/hub", hubTestSkill, "SKILL.md")
	body, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if !strings.Contains(string(body), "name: "+hubTestSkill) {
		t.Errorf("manifest content unexpected:\n%s", body)
	}
	if _, err := os.Stat(filepath.Join(state, "skills/hub", hubTestSkill, ".hugen-checksum")); err != nil {
		t.Errorf("checksum file missing: %v", err)
	}
	// Agent-core skills must NOT be materialised on disk — they
	// live embed-only under the system tier.
	for _, sys := range []string{"_system", "_root", "_mission", "_worker"} {
		path := filepath.Join(state, "skills/hub", sys)
		if _, err := os.Stat(path); err == nil {
			t.Errorf("system-tier skill %q leaked onto hub disk path %s", sys, path)
		}
	}
}

func TestInstallBundledHubSkills_Idempotent(t *testing.T) {
	state := t.TempDir()
	if err := InstallBundledHubSkills(state, discardLogger()); err != nil {
		t.Fatalf("first install: %v", err)
	}
	manifest := filepath.Join(state, "skills/hub", hubTestSkill, "SKILL.md")
	first, err := os.Stat(manifest)
	if err != nil {
		t.Fatalf("first stat: %v", err)
	}

	if err := InstallBundledHubSkills(state, discardLogger()); err != nil {
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

func TestInstallBundledHubSkills_ChecksumMismatchReplaces(t *testing.T) {
	state := t.TempDir()
	if err := InstallBundledHubSkills(state, discardLogger()); err != nil {
		t.Fatalf("install: %v", err)
	}
	checksumPath := filepath.Join(state, "skills/hub", hubTestSkill, ".hugen-checksum")
	if err := os.WriteFile(checksumPath, []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(state, "skills/hub", hubTestSkill, "leftover.txt")
	if err := os.WriteFile(stray, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := InstallBundledHubSkills(state, discardLogger()); err != nil {
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

func TestInstallBundledHubSkills_EmptyStateDir(t *testing.T) {
	if err := InstallBundledHubSkills("", discardLogger()); err == nil {
		t.Fatal("expected error for empty state dir")
	}
}

// TestInstallBundledHubSkills_LegacySystemDirRemoved verifies the
// one-time migration sweep: a pre-split `skills/system/` directory
// (populated by older binaries) is wiped at the next install so
// the agent's mental model matches the on-disk layout.
func TestInstallBundledHubSkills_LegacySystemDirRemoved(t *testing.T) {
	state := t.TempDir()
	legacy := filepath.Join(state, "skills/system")
	if err := os.MkdirAll(filepath.Join(legacy, "_root"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "_root", "SKILL.md"),
		[]byte("# old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := InstallBundledHubSkills(state, discardLogger()); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("legacy skills/system/ should be gone: stat err=%v", err)
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
	manifest := filepath.Join(state, "skills/hub", hubTestSkill, "SKILL.md")
	if _, err := os.Stat(manifest); err != nil {
		t.Errorf("phase did not install hub skill: %v", err)
	}
}
