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

// TestInstallBundledHubSkills_AddsBundleSkillOnExistingInstall
// proves the additive path: an existing install that already has
// one bundled skill installed gets a fresh sibling on the next
// run. Locks the "new binary ships an extra skill" upgrade flow
// against future regressions.
func TestInstallBundledHubSkills_AddsBundleSkillOnExistingInstall(t *testing.T) {
	state := t.TempDir()
	// Seed: pretend the previous binary only shipped hubTestSkill
	// by running a real install then deleting every other skill
	// directory it created.
	if err := InstallBundledHubSkills(state, discardLogger()); err != nil {
		t.Fatalf("seed install: %v", err)
	}
	hubRoot := filepath.Join(state, "skills/hub")
	entries, err := os.ReadDir(hubRoot)
	if err != nil {
		t.Fatalf("readdir seed: %v", err)
	}
	preserved := map[string]struct{}{}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == hubTestSkill {
			continue
		}
		if err := os.RemoveAll(filepath.Join(hubRoot, e.Name())); err != nil {
			t.Fatalf("trim seed: %v", err)
		}
	}
	preserved[hubTestSkill] = struct{}{}

	// Sanity: hubTestSkill still on disk after the trim.
	if _, err := os.Stat(filepath.Join(hubRoot, hubTestSkill, "SKILL.md")); err != nil {
		t.Fatalf("seed witness gone: %v", err)
	}

	// Run install again — should re-add every other bundled skill
	// (treated as "new" by the additive path) without touching the
	// untouched hubTestSkill checksum file.
	checksumBefore, err := os.ReadFile(filepath.Join(hubRoot, hubTestSkill, ".hugen-checksum"))
	if err != nil {
		t.Fatalf("read checksum: %v", err)
	}
	if err := InstallBundledHubSkills(state, discardLogger()); err != nil {
		t.Fatalf("re-install: %v", err)
	}
	checksumAfter, err := os.ReadFile(filepath.Join(hubRoot, hubTestSkill, ".hugen-checksum"))
	if err != nil {
		t.Fatalf("read checksum after: %v", err)
	}
	if string(checksumBefore) != string(checksumAfter) {
		t.Errorf("hubTestSkill checksum changed unexpectedly: %q -> %q",
			checksumBefore, checksumAfter)
	}

	// Verify at least one non-hubTestSkill bundled skill re-appeared
	// — proves the additive path triggers for entries missing from
	// the previous install.
	after, err := os.ReadDir(hubRoot)
	if err != nil {
		t.Fatalf("readdir after: %v", err)
	}
	added := 0
	for _, e := range after {
		if !e.IsDir() || e.Name() == hubTestSkill {
			continue
		}
		added++
	}
	if added == 0 {
		t.Errorf("no sibling bundled skills re-added on re-install")
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
