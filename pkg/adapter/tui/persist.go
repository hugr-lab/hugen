package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/oasdiff/yaml"
)

// settingsFileName is the path under the user's home dir where TUI
// state persists between runs. Phase 5.1c §11 — user-config, NOT
// operator config; absence is non-fatal.
const settingsFileName = ".hugen/tui.yaml"

// tuiSettings is the on-disk schema. Slice 5 only writes
// RecentRoots; slice 6 will add theme / history / panel state.
//
// Backwards-compat note: unknown YAML keys are ignored on read; new
// fields are appended on write so older binaries don't blow up on
// fields they don't understand.
type tuiSettings struct {
	RecentRoots []string `yaml:"recent_roots,omitempty"`
}

// settingsPath resolves the absolute path to ~/.hugen/tui.yaml.
// Empty + error returned when $HOME is unset (rare but possible in
// CI / sandboxed shells); caller treats as "no persistence".
func settingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("tui: cannot resolve home dir: %w", err)
	}
	return filepath.Join(home, settingsFileName), nil
}

// loadSettings reads ~/.hugen/tui.yaml. A missing file returns an
// empty settings struct + nil error (first-run path). Parse errors
// also degrade to empty so a corrupt file never bricks the TUI;
// the caller logs the warning.
func loadSettings() (*tuiSettings, error) {
	path, err := settingsPath()
	if err != nil {
		return &tuiSettings{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &tuiSettings{}, nil
		}
		return &tuiSettings{}, fmt.Errorf("tui: read settings: %w", err)
	}
	var s tuiSettings
	if err := yaml.Unmarshal(raw, &s); err != nil {
		return &tuiSettings{}, fmt.Errorf("tui: parse settings: %w", err)
	}
	return &s, nil
}

// saveSettings rewrites ~/.hugen/tui.yaml atomically (write-to-temp
// + rename) so a crash mid-write never leaves a half-written file.
// Mkdir ensures parent exists on first run.
func saveSettings(s *tuiSettings) error {
	path, err := settingsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("tui: mkdir settings: %w", err)
	}
	out, err := yaml.Marshal(s)
	if err != nil {
		return fmt.Errorf("tui: marshal settings: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return fmt.Errorf("tui: write settings: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("tui: rename settings: %w", err)
	}
	return nil
}

// rememberRoot is a small helper: prepends id if not already in
// the list, caps at maxRememberedRoots so the file stays bounded.
func rememberRoot(existing []string, id string) []string {
	out := []string{id}
	for _, prev := range existing {
		if prev == id {
			continue
		}
		out = append(out, prev)
		if len(out) >= maxRememberedRoots {
			break
		}
	}
	return out
}

// forgetRoot drops id from the list. Pure helper.
func forgetRoot(existing []string, id string) []string {
	out := existing[:0]
	for _, prev := range existing {
		if prev == id {
			continue
		}
		out = append(out, prev)
	}
	return out
}

// dedupedSorted is a defensive helper for tests: returns a stable
// snapshot of the list with duplicates removed. Settings files
// hand-edited by users may carry dupes; the load path tolerates
// them.
func dedupedSorted(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// maxRememberedRoots caps the persisted list. Phase 5.1c default —
// 8 roots is plenty for a daily workflow; older entries fall off
// the tail.
const maxRememberedRoots = 8
