package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hugr-lab/hugen/assets"
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
// Idempotency: each skill directory writes a sentinel
// `.hugen-checksum` file containing a sha-256 over its embedded
// contents. Re-running the installer is a no-op when the checksum
// matches; a mismatch (binary upgraded, payload changed) replaces
// the existing tree.
//
// Reconcile: subdirectories present in the target tree but absent
// from the current embed (skills retired across a version bump)
// are removed at the end of the pass. Local skills under
// `skills/local/` live in a sibling root and are untouched.
//
// Future: when the Hub becomes a real remote source, this
// function is replaced by a Hugr-function-driven sync that fills
// the same on-disk cache.
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
		if err := installOneSkill(name, target, log); err != nil {
			return err
		}
	}
	return reconcileStaleSkills(target, want, log)
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

// reconcileStaleSkills removes any subdirectory under target
// whose name is not in `want` — i.e. a skill that the previous
// binary version installed but the current one no longer ships.
// Errors are logged warn-not-fatal: a stale directory we cannot
// remove (filesystem permission, file open elsewhere) should not
// block the bootstrap.
func reconcileStaleSkills(target string, want map[string]struct{}, log *slog.Logger) error {
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
		path := filepath.Join(target, name)
		if err := os.RemoveAll(path); err != nil {
			log.Warn("bundled skill: failed to remove stale dir",
				"name", name, "path", path, "err", err)
			continue
		}
		log.Info("bundled skill: removed stale dir", "name", name, "path", path)
	}
	return nil
}

func installOneSkill(name, target string, log *slog.Logger) error {
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
	sum := embedChecksum(files)
	dst := filepath.Join(target, name)
	checksumPath := filepath.Join(dst, ".hugen-checksum")
	if existing, err := os.ReadFile(checksumPath); err == nil && strings.TrimSpace(string(existing)) == sum {
		log.Debug("bundled skill up-to-date", "name", name, "sha256", sum)
		return nil
	}
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
	if err := os.WriteFile(checksumPath, []byte(sum+"\n"), 0o644); err != nil {
		return fmt.Errorf("install %s: write checksum: %w", name, err)
	}
	log.Info("bundled skill installed", "name", name, "files", len(files), "sha256", sum)
	return nil
}

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

func embedChecksum(files []embeddedFile) string {
	h := sha256.New()
	for _, f := range files {
		_, _ = h.Write([]byte(f.path))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(f.data)
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// phaseBundledSkills runs the bundled-skill installer (phase 1).
// Materialises the hub-tier bundle to ${StateDir}/skills/hub/
// and removes the legacy ${StateDir}/skills/system/ directory
// from the pre-split layout. Agent-core (`_*`) skills live
// embed-only and skip disk entirely.
func phaseBundledSkills(core *Core) error {
	return InstallBundledHubSkills(core.Cfg.StateDir, core.Logger)
}
