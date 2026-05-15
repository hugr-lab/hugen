package tui

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRememberRoot_PrependsAndDedupes(t *testing.T) {
	cases := []struct {
		name     string
		existing []string
		id       string
		want     []string
	}{
		{"empty", nil, "a", []string{"a"}},
		{"prepend_new", []string{"b", "c"}, "a", []string{"a", "b", "c"}},
		{"move_to_front", []string{"b", "a", "c"}, "a", []string{"a", "b", "c"}},
		{"idempotent_head", []string{"a", "b"}, "a", []string{"a", "b"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := rememberRoot(c.existing, c.id)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("rememberRoot(%v, %q) = %v; want %v", c.existing, c.id, got, c.want)
			}
		})
	}
}

func TestRememberRoot_CapsAtMaxRememberedRoots(t *testing.T) {
	existing := make([]string, maxRememberedRoots+5)
	for i := range existing {
		existing[i] = string(rune('a' + i))
	}
	got := rememberRoot(existing, "Z")
	if len(got) != maxRememberedRoots {
		t.Errorf("len(got) = %d; want %d", len(got), maxRememberedRoots)
	}
	if got[0] != "Z" {
		t.Errorf("got[0] = %q; want Z (most-recent)", got[0])
	}
}

func TestForgetRoot_RemovesAllOccurrences(t *testing.T) {
	got := forgetRoot([]string{"a", "b", "a", "c"}, "a")
	want := []string{"b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("forgetRoot dup-handling: got %v want %v", got, want)
	}
}

// TestSaveLoad_RoundTrip writes a settings file via the real save
// helper (redirecting $HOME to a temp dir so the test does NOT
// touch the operator's real ~/.hugen/tui.yaml), reads it back, and
// asserts equality. Phase 5.1c §11 — settings are a UX nicety;
// the round-trip property guards against silent schema drift.
func TestSaveLoad_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	in := &tuiSettings{RecentRoots: []string{"ses-aaa", "ses-bbb"}}
	if err := saveSettings(in); err != nil {
		t.Fatalf("save: %v", err)
	}
	// File must exist on disk under the redirected $HOME.
	if _, err := os.Stat(filepath.Join(dir, ".hugen", "tui.yaml")); err != nil {
		t.Fatalf("expected settings file at %s/.hugen/tui.yaml: %v", dir, err)
	}
	out, err := loadSettings()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !reflect.DeepEqual(out.RecentRoots, in.RecentRoots) {
		t.Errorf("round-trip mismatch: got %v want %v", out.RecentRoots, in.RecentRoots)
	}
}

func TestLoadSettings_MissingFileIsBenign(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	out, err := loadSettings()
	if err != nil {
		t.Fatalf("missing file should be benign; got err=%v", err)
	}
	if len(out.RecentRoots) != 0 {
		t.Errorf("missing file produced non-empty settings: %+v", out)
	}
}

func TestSaveSettings_OwnerOnlyPerms(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	if err := saveSettings(&tuiSettings{RecentRoots: []string{"ses-a"}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dir, ".hugen", "tui.yaml"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// File perms: only owner may read/write.
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("tui.yaml perms = %o; want 0600 (S1 — chat history must not be world-readable)", perm)
	}
	di, err := os.Stat(filepath.Join(dir, ".hugen"))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf(".hugen/ perms = %o; want 0700", perm)
	}
}

func TestLoadSettings_CorruptFileDegradesToDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, ".hugen"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".hugen", "tui.yaml"),
		[]byte("not: valid: yaml: at: all\n  - or maybe?\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	out, _ := loadSettings()
	if len(out.RecentRoots) != 0 {
		t.Errorf("corrupt file should yield empty settings; got %+v", out)
	}
}
