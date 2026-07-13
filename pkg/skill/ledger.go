package skill

// ledger.go — the installed-tier ledger (spec-skills-distribution §3).
//
// `${STATE}/skills/hub/.installed.json` records, per hub-tier skill name, the
// canonical bundle hash currently on disk, how it got there (origin), and
// when. It is the single lifecycle authority the two writers coordinate on:
//
//   - the boot seed (pkg/runtime/bundled_skills.go) — writes `seed` entries,
//     and crucially reads the ledger to avoid clobbering a marketplace-
//     delivered upgrade back to the embedded bytes on every restart;
//   - the background reconciler (pkg/runtime) — writes `desired` (admin
//     desired-set) / `self` (skill:install) entries on download, and removes
//     entries on retire.
//
// The ledger file is a dotfile at the hub root, so it never folds into any
// skill's BundleHash (which excludes dot-segments).

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// InstallOrigin records how a hub-tier bundle came to be on disk. Named
// distinctly from the tier [Origin] (OriginSystem/OriginHub/…) — this is the
// per-bundle install provenance inside the hub tier, a different axis.
type InstallOrigin string

const (
	// InstallSeed — materialised from the binary's embedded bundle. The seed
	// may overwrite/adopt/retire only its own `seed` entries.
	InstallSeed InstallOrigin = "seed"
	// InstallDesired — delivered by the admin desired-set (skills.install /
	// agent_type config) via the reconciler. Refused by an explicit remove
	// ("managed by the admin"); drop it from the desired-set instead.
	InstallDesired InstallOrigin = "desired"
	// InstallSelf — self-installed via the skill:install tool. Removed only
	// by an explicit skill:remove.
	InstallSelf InstallOrigin = "self"
)

// LedgerEntry is one installed-tier record.
type LedgerEntry struct {
	// Hash is the canonical [BundleHash] of the bundle currently on disk.
	Hash string `json:"hash"`
	// Origin is how the bundle got here (seed | desired | self).
	Origin InstallOrigin `json:"origin"`
	// InstalledAt is an RFC3339 timestamp of the last write (provenance only;
	// never load-bearing for a decision, so clock skew is harmless).
	InstalledAt string `json:"installed_at,omitempty"`
}

// ledgerFileName is the on-disk ledger basename (a dotfile → excluded from
// every BundleHash by the dot-segment rule).
const ledgerFileName = ".installed.json"

// Ledger is the in-memory view of `${hubDir}/.installed.json`. Not safe for
// concurrent mutation across goroutines on its own — the seed runs before the
// reconciler goroutine starts, and the reconciler serialises its own passes —
// but the accessors take a mutex so a Save racing a read stays coherent.
type Ledger struct {
	path string

	mu      sync.Mutex
	entries map[string]LedgerEntry
}

// LoadLedger reads the ledger at `${hubDir}/.installed.json`. A missing file
// yields an empty (not-yet-persisted) ledger — the first-boot case. A present
// but corrupt file is a hard error: silently discarding it would re-seed over
// marketplace state (the exact flip-flop §3 guards against), so the caller
// must decide (today: log + treat as empty is the caller's call, not ours).
func LoadLedger(hubDir string) (*Ledger, error) {
	l := &Ledger{
		path:    filepath.Join(hubDir, ledgerFileName),
		entries: map[string]LedgerEntry{},
	}
	b, err := os.ReadFile(l.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return l, nil
		}
		return nil, fmt.Errorf("read ledger %s: %w", l.path, err)
	}
	if len(b) == 0 {
		return l, nil
	}
	if err := json.Unmarshal(b, &l.entries); err != nil {
		return nil, fmt.Errorf("parse ledger %s: %w", l.path, err)
	}
	if l.entries == nil {
		l.entries = map[string]LedgerEntry{}
	}
	return l, nil
}

// Get returns the entry for name and whether it exists.
func (l *Ledger) Get(name string) (LedgerEntry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[name]
	return e, ok
}

// Set records (or replaces) the entry for name.
func (l *Ledger) Set(name string, e LedgerEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries[name] = e
}

// Delete removes the entry for name (no-op when absent).
func (l *Ledger) Delete(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, name)
}

// Names returns the recorded skill names in sorted order.
func (l *Ledger) Names() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.entries))
	for n := range l.entries {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Save atomically writes the ledger (temp file + rename). Creates the hub dir
// if needed so a first-boot save on a pristine state dir succeeds.
func (l *Ledger) Save() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return fmt.Errorf("ledger mkdir: %w", err)
	}
	b, err := json.MarshalIndent(l.entries, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal ledger: %w", err)
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("write ledger tmp: %w", err)
	}
	if err := os.Rename(tmp, l.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("commit ledger: %w", err)
	}
	return nil
}
