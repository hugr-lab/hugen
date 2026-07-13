package runtime

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hugr-lab/hugen/assets"
	"github.com/hugr-lab/hugen/pkg/skill"
)

// hubSkillsSubdir is the on-disk directory under StateDir where
// hub-tier skills land. Today the runtime fills it from the
// binary's embedded bundle (assets/skills/); a future phase will
// replace [InstallBundledHubSkills] with a remote Hugr function
// call against the deployment's Hub that fetches the per-agent-
// type bundle. The on-disk path stays the cache the SkillStore
// reads through.
const hubSkillsSubdir = "skills/hub"

// legacySystemSkillsSubdir is the pre-split path where bundled
// skills (system + hub) used to share a directory. After the
// system/hub split it is cleaned up at boot once and never
// touched again.
const legacySystemSkillsSubdir = "skills/system"

// InstallBundledHubSkills copies every top-level entry under
// assets/skills/ onto disk at ${stateDir}/skills/hub/<name>/.
// These are the admin-delivered extensions (`hugr-data`,
// `analyst`, `duckdb-data`, `duckdb-docs`, `python-runner`)
// shipped with the binary. The agent-core skills (`_root`,
// `_mission`, …) live under assets/system/ and are served
// embed-only via the SkillStore's system backend — they never
// touch disk.
//
// Idempotency + ledger-awareness (spec-skills-distribution §3): the
// install decision is driven by the installed-tier ledger
// (`${target}/.installed.json`), NOT a blind checksum compare. A skill
// dir is (re)written from the embed only when there is no ledger entry
// OR the entry is `seed` AND its recorded hash equals the embed hash.
// A `desired`/`self` entry (a marketplace/self install) is never
// clobbered, and a `seed` entry whose hash has diverged from the embed
// (a marketplace upgrade landed in place) is left alone — this is what
// stops the restart flip-flop of downgrade-then-reupgrade. Each written
// dir also carries a `.hugen-checksum` sentinel = the canonical
// [skill.BundleHash] (§2's fourth hash point; human-facing marker).
//
// Reconcile: `seed`-origin subdirectories present on disk but absent
// from the current embed (skills retired across a version bump) are
// removed at the end of the pass together with their ledger entry;
// `desired`/`self` dirs are the reconciler's to retire, never the
// seed's. Local skills under `skills/local/` live in a sibling root and
// are untouched.
//
// The embed is a seed/fallback, never a live source (SD5): the
// reconciler fills upgrades from the marketplace; this function only
// guarantees a non-empty offline baseline.
func InstallBundledHubSkills(stateDir string, log *slog.Logger) error {
	if stateDir == "" {
		return fmt.Errorf("install bundled hub skills: empty state dir")
	}
	if err := cleanupLegacySystemSkillsDir(stateDir, log); err != nil {
		// Cleanup failure is warn-not-fatal: stale leftovers in
		// the legacy path don't poison the new install.
		log.Warn("legacy system skills cleanup", "err", err)
	}
	target := filepath.Join(stateDir, hubSkillsSubdir)
	if err := os.MkdirAll(target, 0o755); err != nil {
		return fmt.Errorf("install bundled hub skills: %w", err)
	}

	ledger, err := skill.LoadLedger(target)
	if err != nil {
		// A corrupt ledger must not brick boot. Log loudly and continue with
		// an empty ledger: the worst case is a one-time re-seed, and the
		// reconciler re-establishes marketplace state on its first pass.
		log.Warn("install bundled hub skills: ledger unreadable — treating as empty", "err", err)
		ledger, _ = skill.LoadLedger(filepath.Join(target, "___nonexistent___"))
	}
	adoptPreLedgerDirs(target, ledger, log)

	entries, err := fs.ReadDir(assets.SkillsFS, "skills")
	if err != nil {
		return fmt.Errorf("install bundled hub skills: read embed: %w", err)
	}
	want := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		want[name] = struct{}{}
		if err := installOneSkill(name, target, ledger, log); err != nil {
			return err
		}
	}
	if err := reconcileStaleSkills(target, want, ledger, log); err != nil {
		return err
	}
	if err := ledger.Save(); err != nil {
		return fmt.Errorf("install bundled hub skills: save ledger: %w", err)
	}
	return nil
}

// adoptPreLedgerDirs handles the first-boot-on-an-old-state-dir case (§3):
// when a hub-tier bundle dir exists on disk but has no ledger entry, adopt it
// as `seed` at its current on-disk hash. This covers a state dir written by a
// pre-ledger binary. (A pre-ledger self-install would be misclassified as
// seed — an accepted one-time cost; none exist in the field today.) Entries
// already in the ledger are left untouched.
func adoptPreLedgerDirs(target string, ledger *skill.Ledger, log *slog.Logger) {
	dir, err := os.ReadDir(target)
	if err != nil {
		return // missing target → nothing to adopt
	}
	for _, e := range dir {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if _, ok := ledger.Get(name); ok {
			continue
		}
		hash := onDiskBundleHash(filepath.Join(target, name))
		if hash == "" {
			continue // not a readable bundle
		}
		ledger.Set(name, skill.LedgerEntry{Hash: hash, Origin: skill.InstallSeed, InstalledAt: nowStamp()})
		log.Info("bundled skill: adopted pre-ledger dir as seed", "name", name, "hash", hash)
	}
}

// cleanupLegacySystemSkillsDir removes `${stateDir}/skills/system/`
// once after the split. Before 2026-05-13 the bundled installer
// dropped EVERY skill — both the `_*` agent-core set (now
// embed-only) and the hub-tier extensions (now under
// `skills/hub/`) — into this single directory. With both moved
// elsewhere it is purely orphaned state; deleting it on boot
// keeps the on-disk surface aligned with the runtime mental model.
// Returns nil on a fresh install where the directory never
// existed.
func cleanupLegacySystemSkillsDir(stateDir string, log *slog.Logger) error {
	legacy := filepath.Join(stateDir, legacySystemSkillsSubdir)
	info, err := os.Stat(legacy)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat %s: %w", legacy, err)
	}
	if !info.IsDir() {
		return nil
	}
	if err := os.RemoveAll(legacy); err != nil {
		return fmt.Errorf("remove %s: %w", legacy, err)
	}
	log.Info("legacy skills/system/ removed (split into system+hub tiers)",
		"path", legacy)
	return nil
}

// reconcileStaleSkills removes a subdirectory under target whose name is
// not in `want` (the current embed set) ONLY when its ledger entry is
// `seed` (or it has no entry — a seed-ish orphan). A `desired`/`self`
// dir is a marketplace/self install that the embed set knows nothing
// about; retiring it is the reconciler's job, so it is left. Removing a
// retired seed also drops its ledger entry. Errors are warn-not-fatal.
//
// Simplification (§3 folds "absent from the catalog" into retirement):
// the seed phase cannot read the catalog, so it retires a
// no-longer-embedded seed unconditionally; if the marketplace still
// offers that skill the reconciler re-installs it (as desired/self) on
// its next pass. A brief gap on a version bump is acceptable.
func reconcileStaleSkills(target string, want map[string]struct{}, ledger *skill.Ledger, log *slog.Logger) error {
	dir, err := os.ReadDir(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("reconcile stale skills: read dir %s: %w", target, err)
	}
	for _, e := range dir {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if _, keep := want[name]; keep {
			continue
		}
		if entry, ok := ledger.Get(name); ok && entry.Origin != skill.InstallSeed {
			log.Debug("bundled skill: keep non-seed dir retired from embed",
				"name", name, "origin", entry.Origin)
			continue // marketplace/self install — reconciler's to retire
		}
		path := filepath.Join(target, name)
		if err := os.RemoveAll(path); err != nil {
			log.Warn("bundled skill: failed to remove stale dir",
				"name", name, "path", path, "err", err)
			continue
		}
		ledger.Delete(name)
		log.Info("bundled skill: removed stale seed dir", "name", name, "path", path)
	}
	return nil
}

// installOneSkill materialises one embedded bundle onto disk when the
// ledger says it should be (see [InstallBundledHubSkills] for the rule),
// then records/updates its `seed` ledger entry. The install decision
// keys on the canonical whole-bundle hash of the embed sub-tree, which
// equals the on-disk BundleHash for identical content (dotfiles — the
// sentinel + ledger — excluded on both sides).
func installOneSkill(name, target string, ledger *skill.Ledger, log *slog.Logger) error {
	embedRoot := "skills/" + name
	files, err := collectEmbedFiles(embedRoot)
	if err != nil {
		return fmt.Errorf("install %s: %w", name, err)
	}
	if len(files) == 0 {
		// Empty placeholder dir (e.g. hugr-data lands in US2);
		// skip until it has content.
		return nil
	}
	sub, err := fs.Sub(assets.SkillsFS, embedRoot)
	if err != nil {
		return fmt.Errorf("install %s: sub-fs: %w", name, err)
	}
	embedHash, err := skill.BundleHash(sub)
	if err != nil {
		return fmt.Errorf("install %s: hash embed: %w", name, err)
	}

	// Decide whether the on-disk bundle SHOULD be the embed (§3): write when
	// there is no ledger entry (fresh) OR the entry is `seed` at exactly the
	// embed hash (the embed is the intended content — re-materialise if the
	// dir was deleted/corrupted out of band). Skip otherwise:
	//   - a `desired`/`self` entry: a marketplace/self install owns the name;
	//   - a `seed` entry whose hash diverged from the embed: a marketplace
	//     upgrade landed in place, so leaving the embed out kills the restart
	//     flip-flop of downgrade-then-reupgrade.
	// Known limitation: a genuine embed-content bump of an already-seeded
	// skill is indistinguishable from a marketplace upgrade by (origin, hash)
	// alone, so it is also skipped — binary-embed upgrades flow through the
	// hub re-seed → catalog → reconciler path (SD5), not in place. A pure
	// local (no-marketplace) agent must clear the ledger entry to force one.
	if entry, ok := ledger.Get(name); ok {
		if entry.Origin != skill.InstallSeed {
			log.Debug("bundled skill: skip (owned by reconciler/self)", "name", name, "origin", entry.Origin)
			return nil
		}
		if entry.Hash != embedHash {
			log.Debug("bundled skill: skip (seed hash diverged — marketplace-owned upgrade)",
				"name", name, "ledger_hash", entry.Hash, "embed_hash", embedHash)
			return nil
		}
		// seed @ embed hash → ensure it is materialised. If the on-disk
		// content already matches, this is a no-op (mtime preserved).
		if onDiskBundleHash(filepath.Join(target, name)) == embedHash {
			log.Debug("bundled skill up-to-date", "name", name, "hash", embedHash)
			return nil
		}
	}

	// Fresh install, or a seed@embed whose on-disk content drifted (deleted
	// dir / stray file) → (re)write from the embed.
	dst := filepath.Join(target, name)
	if err := os.RemoveAll(dst); err != nil {
		return fmt.Errorf("install %s: clean: %w", name, err)
	}
	for _, f := range files {
		rel := strings.TrimPrefix(f.path, embedRoot+"/")
		out := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return fmt.Errorf("install %s: mkdir %s: %w", name, out, err)
		}
		if err := os.WriteFile(out, f.data, 0o644); err != nil {
			return fmt.Errorf("install %s: write %s: %w", name, out, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dst, ".hugen-checksum"), []byte(embedHash+"\n"), 0o644); err != nil {
		return fmt.Errorf("install %s: write checksum: %w", name, err)
	}
	ledger.Set(name, skill.LedgerEntry{Hash: embedHash, Origin: skill.InstallSeed, InstalledAt: nowStamp()})
	log.Info("bundled skill installed", "name", name, "files", len(files), "hash", embedHash)
	return nil
}

// onDiskBundleHash returns the canonical [skill.BundleHash] of an on-disk
// bundle dir, or "" on any read error (caller treats "" as "unknown").
func onDiskBundleHash(dir string) string {
	h, err := skill.BundleHash(os.DirFS(dir))
	if err != nil {
		return ""
	}
	return h
}

// nowStamp returns an RFC3339 UTC timestamp for a ledger InstalledAt field
// (provenance only — never load-bearing, so it need not be monotonic).
func nowStamp() string { return time.Now().UTC().Format(time.RFC3339) }

type embeddedFile struct {
	path string
	data []byte
}

// collectEmbedFiles walks the embed.FS tree under root and returns
// every file in deterministic order (by path) so checksums and
// install order are reproducible. Wraps assets.SkillsFS — kept for
// historical call sites; new bundled installers should use
// [collectEmbedFilesFS] with their own embed.FS handle.
func collectEmbedFiles(root string) ([]embeddedFile, error) {
	return collectEmbedFilesFS(assets.SkillsFS, root)
}

// collectEmbedFilesFS is the generic walk used by every bundled-
// asset installer. Caller passes the embed.FS handle (skills,
// constitution, …) and the root path within it.
func collectEmbedFilesFS(efs fs.FS, root string) ([]embeddedFile, error) {
	var out []embeddedFile
	err := fs.WalkDir(efs, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, rerr := fs.ReadFile(efs, path)
		if rerr != nil {
			return rerr
		}
		out = append(out, embeddedFile{path: path, data: data})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out, nil
}

// phaseBundledSkills runs the bundled-skill installer (phase 1).
// Materialises the hub-tier bundle to ${StateDir}/skills/hub/
// and removes the legacy ${StateDir}/skills/system/ directory
// from the pre-split layout. Agent-core (`_*`) skills live
// embed-only and skip disk entirely.
func phaseBundledSkills(core *Core) error {
	return InstallBundledHubSkills(core.Cfg.StateDir, core.Logger)
}
