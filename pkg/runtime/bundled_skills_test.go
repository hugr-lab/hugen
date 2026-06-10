package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/assets"
	"github.com/hugr-lab/hugen/pkg/skill"
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
	for _, sys := range []string{"_system", "_root", "_mission", "_worker", "_mission_worker", "_admin"} {
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

// TestRootManifest_AsyncSpawnHint pins the _root on_tool_result hint
// against the shape the runtime actually produces: it must fire on an
// async spawn_mission result (status "running" — the juncture where a
// weak model fabricated mission results, dogfood 2026-06-10) and stay
// silent on a sync spawn whose result carries the real outcome.
func TestRootManifest_AsyncSpawnHint(t *testing.T) {
	body, err := assets.SystemSkillsFS.ReadFile("system/_root/SKILL.md")
	if err != nil {
		t.Fatalf("read embedded _root: %v", err)
	}
	m, err := skill.Parse(body)
	if err != nil {
		t.Fatalf("parse _root: %v", err)
	}
	if len(m.Hugen.Hints) == 0 {
		t.Fatal("_root must declare the async-spawn on_tool_result hint")
	}
	const asyncResult = `{"result":{"depth":1,"mission_id":"ses-1","name":"roads","session_id":"ses-1","status":"running"},"tool_id":"1"}`
	const syncResult = `{"result":{"depth":1,"mission_id":"ses-1","name":"roads","session_id":"ses-1","status":"completed","handoff":{...}},"tool_id":"1"}`
	var fired string
	for _, h := range m.Hugen.Hints {
		// Tool name in the model-visible `_` form — matching is
		// separator-insensitive.
		if msg := h.MatchToolResult("session_spawn_mission", "", asyncResult); msg != "" {
			fired = msg
		}
		if msg := h.MatchToolResult("session_spawn_mission", "", syncResult); msg != "" {
			t.Errorf("hint must not fire on a sync (completed) spawn result; got %q", msg)
		}
	}
	if !strings.Contains(fired, "RUNNING in the background") {
		t.Errorf("async spawn result must surface the announce-and-stop hint; got %q", fired)
	}
}
