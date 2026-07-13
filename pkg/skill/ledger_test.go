package skill

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLedger_MissingFileIsEmpty(t *testing.T) {
	dir := t.TempDir()
	l, err := LoadLedger(dir)
	if err != nil {
		t.Fatalf("LoadLedger on empty dir: %v", err)
	}
	if got := l.Names(); len(got) != 0 {
		t.Fatalf("fresh ledger has entries: %v", got)
	}
	if _, ok := l.Get("anything"); ok {
		t.Fatal("fresh ledger reported a hit")
	}
}

func TestLedger_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	l, err := LoadLedger(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	l.Set("hugr-data", LedgerEntry{Hash: "sha256:aaa", Origin: InstallSeed, InstalledAt: "2026-07-13T00:00:00Z"})
	l.Set("analyst", LedgerEntry{Hash: "sha256:bbb", Origin: InstallDesired})
	if err := l.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	// The persisted file is a dotfile at the hub root, so it never folds into
	// a bundle hash.
	if _, err := os.Stat(filepath.Join(dir, ".installed.json")); err != nil {
		t.Fatalf("ledger not persisted: %v", err)
	}

	l2, err := LoadLedger(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, ok := l2.Get("hugr-data")
	if !ok || got.Hash != "sha256:aaa" || got.Origin != InstallSeed {
		t.Fatalf("reloaded hugr-data = %+v ok=%v", got, ok)
	}
	if got, ok := l2.Get("analyst"); !ok || got.Origin != InstallDesired {
		t.Fatalf("reloaded analyst = %+v ok=%v", got, ok)
	}
	if names := l2.Names(); len(names) != 2 || names[0] != "analyst" || names[1] != "hugr-data" {
		t.Fatalf("Names not sorted/complete: %v", names)
	}
}

func TestLedger_DeleteAndReplace(t *testing.T) {
	dir := t.TempDir()
	l, _ := LoadLedger(dir)
	l.Set("x", LedgerEntry{Hash: "sha256:1", Origin: InstallSelf})
	l.Set("x", LedgerEntry{Hash: "sha256:2", Origin: InstallSelf}) // replace
	if got, _ := l.Get("x"); got.Hash != "sha256:2" {
		t.Fatalf("replace failed: %+v", got)
	}
	l.Delete("x")
	if _, ok := l.Get("x"); ok {
		t.Fatal("delete left the entry")
	}
	l.Delete("x") // idempotent
}

// TestSourceRank_AuthoredShadowsHub pins the same-name resolution order
// getByName relies on: an agent's own authored skill wins over the admin-
// delivered hub bundle, and both beat unknown sources.
func TestSourceRank_AuthoredShadowsHub(t *testing.T) {
	if sourceRank("authored") >= sourceRank("hub") {
		t.Errorf("authored (%d) must outrank hub (%d)", sourceRank("authored"), sourceRank("hub"))
	}
	if sourceRank("hub") >= sourceRank("unknown-source") {
		t.Errorf("hub (%d) must outrank an unknown source (%d)", sourceRank("hub"), sourceRank("unknown-source"))
	}
}

func TestLedger_CorruptFileErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".installed.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadLedger(dir); err == nil {
		t.Fatal("corrupt ledger loaded without error — would silently re-seed over marketplace state")
	}
}
