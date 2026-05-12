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

// systemSkillsSubdir is the directory under StateDir where bundled
// skills are materialised on disk. SkillStore consumes this path
// via the system:// backend.
const systemSkillsSubdir = "skills/system"

// InstallBundledSkills copies every top-level entry under
// assets/skills/ onto disk at ${stateDir}/skills/system/<name>/.
//
// Idempotency: each skill directory writes a sentinel
// `.hugen-checksum` file containing a sha-256 over its embedded
// contents. Re-running the installer is a no-op when the checksum
// matches; a mismatch (binary upgraded, payload changed) replaces
// the existing tree.
//
// Reconcile: subdirectories present in the target tree but absent
// from the current embed (typical for legacy skills retired
// across a version bump — e.g. `_subagent` retired in 4.2.2) are
// removed at the end of the pass. Without this the strict
// manifest validator (phase 4.2.2 enforces `autoload_for` ⊆
// {root, mission, worker}) rejects the entire SkillStore.List on
// the first parse error and operators end up with empty tool
// surfaces. Local / community skills live in sibling roots
// (`skills/local/`, `skills/community/`) and are untouched.
func InstallBundledSkills(stateDir string, log *slog.Logger) error {
	if stateDir == "" {
		return fmt.Errorf("install bundled skills: empty state dir")
	}
	target := filepath.Join(stateDir, systemSkillsSubdir)
	if err := os.MkdirAll(target, 0o755); err != nil {
		return fmt.Errorf("install bundled skills: %w", err)
	}
	entries, err := fs.ReadDir(assets.SkillsFS, "skills")
	if err != nil {
		return fmt.Errorf("install bundled skills: read embed: %w", err)
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
// install order are reproducible.
func collectEmbedFiles(root string) ([]embeddedFile, error) {
	var out []embeddedFile
	err := fs.WalkDir(assets.SkillsFS, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, rerr := fs.ReadFile(assets.SkillsFS, path)
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
// Reads cfg.StateDir; writes <StateDir>/skills/system/<name>/ on
// disk. Adds no resource to Core — the SkillStore mounts this path
// in phase 7.
func phaseBundledSkills(core *Core) error {
	return InstallBundledSkills(core.Cfg.StateDir, core.Logger)
}
