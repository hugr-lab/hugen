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

// TestInstallBundledHubSkills_WritesLedger proves the seed persists a
// `.installed.json` recording the seeded skill as origin=seed at its
// canonical embed hash — the record the reconciler coordinates on.
func TestInstallBundledHubSkills_WritesLedger(t *testing.T) {
	state := t.TempDir()
	if err := InstallBundledHubSkills(state, discardLogger()); err != nil {
		t.Fatalf("install: %v", err)
	}
	hubRoot := filepath.Join(state, "skills/hub")
	ledger, err := skill.LoadLedger(hubRoot)
	if err != nil {
		t.Fatalf("load ledger: %v", err)
	}
	entry, ok := ledger.Get(hubTestSkill)
	if !ok {
		t.Fatalf("ledger missing %q", hubTestSkill)
	}
	if entry.Origin != skill.InstallSeed {
		t.Errorf("origin = %q, want seed", entry.Origin)
	}
	// The ledger hash must equal the canonical hash of the materialised dir.
	onDisk := onDiskBundleHash(filepath.Join(hubRoot, hubTestSkill))
	if entry.Hash != onDisk || onDisk == "" {
		t.Errorf("ledger hash %q != on-disk hash %q", entry.Hash, onDisk)
	}
}

// TestInstallBundledHubSkills_DoesNotClobberDesired proves the flip-flop
// fix: a marketplace (desired) install recorded in the ledger is never
// overwritten by the embed seed, even when a same-named embed exists.
func TestInstallBundledHubSkills_DoesNotClobberDesired(t *testing.T) {
	state := t.TempDir()
	hubRoot := filepath.Join(state, "skills/hub")
	if err := os.MkdirAll(hubRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	// Pre-seed a desired install of hubTestSkill with sentinel content + a
	// ledger entry claiming a marketplace-delivered version.
	dst := filepath.Join(hubRoot, hubTestSkill)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dst, "MARKETPLACE_VERSION")
	if err := os.WriteFile(marker, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "SKILL.md"), []byte("name: "+hubTestSkill+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ledger, _ := skill.LoadLedger(hubRoot)
	ledger.Set(hubTestSkill, skill.LedgerEntry{Hash: "sha256:marketplace", Origin: skill.InstallDesired})
	if err := ledger.Save(); err != nil {
		t.Fatal(err)
	}

	if err := InstallBundledHubSkills(state, discardLogger()); err != nil {
		t.Fatalf("install: %v", err)
	}
	// The marketplace marker must survive — the embed did not clobber it.
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("desired install was clobbered by the seed: %v", err)
	}
	// And the ledger still records it as desired.
	after, _ := skill.LoadLedger(hubRoot)
	if e, ok := after.Get(hubTestSkill); !ok || e.Origin != skill.InstallDesired {
		t.Errorf("ledger origin flipped: %+v ok=%v", e, ok)
	}
}

// TestInstallBundledHubSkills_SeedDivergedHashNotOverwritten proves that a
// `seed` entry whose recorded hash has diverged from the embed (a
// marketplace upgrade landed in place) is left alone on a restart.
func TestInstallBundledHubSkills_SeedDivergedHashNotOverwritten(t *testing.T) {
	state := t.TempDir()
	hubRoot := filepath.Join(state, "skills/hub")
	if err := os.MkdirAll(filepath.Join(hubRoot, hubTestSkill), 0o755); err != nil {
		t.Fatal(err)
	}
	upgraded := filepath.Join(hubRoot, hubTestSkill, "UPGRADED_BODY")
	if err := os.WriteFile(upgraded, []byte("catalog"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hubRoot, hubTestSkill, "SKILL.md"), []byte("name: "+hubTestSkill+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ledger, _ := skill.LoadLedger(hubRoot)
	// origin=seed but hash != any embed hash → records a marketplace upgrade.
	ledger.Set(hubTestSkill, skill.LedgerEntry{Hash: "sha256:catalogupgrade", Origin: skill.InstallSeed})
	if err := ledger.Save(); err != nil {
		t.Fatal(err)
	}
	if err := InstallBundledHubSkills(state, discardLogger()); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := os.Stat(upgraded); err != nil {
		t.Errorf("marketplace-upgraded seed body was clobbered back to embed: %v", err)
	}
}

// TestInstallBundledHubSkills_RematerialisesDeletedSeed proves that a
// seed@embed skill whose dir is deleted out of band (ledger entry intact)
// is re-materialised on the next seed pass.
func TestInstallBundledHubSkills_RematerialisesDeletedSeed(t *testing.T) {
	state := t.TempDir()
	if err := InstallBundledHubSkills(state, discardLogger()); err != nil {
		t.Fatalf("first install: %v", err)
	}
	dst := filepath.Join(state, "skills/hub", hubTestSkill)
	if err := os.RemoveAll(dst); err != nil { // out-of-band delete, ledger untouched
		t.Fatal(err)
	}
	if err := InstallBundledHubSkills(state, discardLogger()); err != nil {
		t.Fatalf("second install: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "SKILL.md")); err != nil {
		t.Errorf("deleted seed not re-materialised: %v", err)
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
