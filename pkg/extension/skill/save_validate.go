package skill

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing/fstest"

	"github.com/hugr-lab/hugen/pkg/extension"
	wsext "github.com/hugr-lab/hugen/pkg/extension/workspace"
	skillpkg "github.com/hugr-lab/hugen/pkg/skill"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// ErrUnknownToolName is wrapped into the skill:save error when a task
// manifest's allowed_tools_default names a tool that is absent from
// the live registry — the dominant run-2 dogfood failure (the model
// invented `hugr-data:execute` / `python-runner:run`, which are SKILL
// names, not provider:tool names). errors.Is-checkable so the model
// reads a typed, actionable verdict and self-corrects.
var ErrUnknownToolName = errors.New("skill_unknown_tool")

// errNoToolCatalogue marks the registry as unavailable (fixture
// session without a wired ToolManager). The registry-backed half of
// the authoring check skips rather than blocking a legitimate save.
var errNoToolCatalogue = errors.New("skill: tool catalogue unavailable")

// validateAuthoring runs every save-time check over a parsed manifest
// before publish — the same verdict for skill:validate dry-runs and
// skill:save registrations. Three layers:
//
//   - autoload-reserved (local skills load on demand);
//   - task-block placement (skillpkg.ValidateTaskAuthoring — the pure,
//     manifest-internal half of D2);
//   - allowed_tools_default names exist in the live registry (the
//     registry-backed half, below).
//
// Every error is actionable and typed so the agent fixes the files in
// its workspace and re-calls skill:save.
func (h *SessionSkill) validateAuthoring(ctx context.Context, m skillpkg.Manifest) error {
	if m.Hugen.Autoload {
		return fmt.Errorf("skill:save: %w — drop `metadata.hugen.autoload` from the manifest and re-save (autoload is reserved for system / admin skills compiled into the binary; local skills load on demand)", skillpkg.ErrAutoloadReserved)
	}
	if err := skillpkg.ValidateTaskAuthoring(m); err != nil {
		return fmt.Errorf("skill:save: %w", err)
	}
	if m.Hugen.Task.Eligible && len(m.Hugen.Task.AllowedToolsDefault) > 0 {
		if err := h.validateToolNames(ctx, m.Hugen.Task.AllowedToolsDefault); err != nil {
			return fmt.Errorf("skill:save: %w", err)
		}
	}
	return nil
}

// validateToolNames checks every allowed_tools_default entry against
// the live tool registry. Lenient by design — an entry is rejected
// only when it cannot possibly resolve to a real tool: a bad provider
// (the skill-name-as-provider mistake) or a real provider with a
// non-existent tool name. Wildcards (`provider:prefix*`, bare
// `provider`) pass when the provider is real. Returns nil when the
// catalogue is unavailable (fixture) so the check never blocks a save
// it cannot evaluate.
func (h *SessionSkill) validateToolNames(ctx context.Context, declared []string) error {
	cat, err := h.toolCatalogue(ctx)
	if err != nil {
		return nil
	}
	realTools := make(map[string]struct{})
	providers := make(map[string]struct{})
	for _, pc := range cat {
		providers[pc.Name] = struct{}{}
		for _, t := range pc.Tools {
			realTools[t.Name] = struct{}{}
		}
	}

	var problems []string
	seen := make(map[string]struct{})
	for _, raw := range declared {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		if _, dup := seen[entry]; dup {
			continue
		}
		seen[entry] = struct{}{}
		if toolEntryMatches(entry, realTools, providers) {
			continue
		}
		problems = append(problems, toolNameHint(entry, providers))
	}
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return fmt.Errorf("%w: %s — allowed_tools_default entries MUST be exact provider:tool names from the live registry; run tool:providers then tool:tools(<provider>) to look them up, never invent names",
		ErrUnknownToolName, strings.Join(problems, "; "))
}

// toolEntryMatches reports whether an allowed_tools_default entry can
// resolve against the registry. Exact provider:tool wins; a `*`-suffix
// matches by prefix; a bare provider (no colon) matches when the
// provider is real. Mirrors the exact-name + '*'-wildcard grant
// semantics (see skillpkg.ToolGrant.RequiresApproval).
func toolEntryMatches(entry string, realTools, providers map[string]struct{}) bool {
	if _, ok := realTools[entry]; ok {
		return true
	}
	i := strings.IndexByte(entry, ':')
	if i <= 0 {
		// Bare provider name — lenient "all tools of provider".
		_, ok := providers[entry]
		return ok
	}
	name := entry[i+1:]
	if strings.HasSuffix(name, "*") {
		pfx := entry[:len(entry)-1] // drop the trailing '*'
		for real := range realTools {
			if strings.HasPrefix(real, pfx) {
				return true
			}
		}
	}
	return false
}

// toolNameHint builds a targeted, single-entry diagnostic. When the
// provider is real the tool name is the problem; when the provider is
// unknown the classic cause is naming a SKILL as a provider — the
// message says so without a per-entry store lookup (the worse the
// model's guess, the more bad entries; a DB round-trip each just to
// sharpen error text is wasteful, and the wording steers correctly
// either way).
func toolNameHint(entry string, providers map[string]struct{}) string {
	prov, name := entry, ""
	if i := strings.IndexByte(entry, ':'); i > 0 {
		prov, name = entry[:i], entry[i+1:]
	}
	if _, ok := providers[prov]; ok {
		return fmt.Sprintf("%q: provider %q has no tool %q — run tool:tools(%q) for its real tool names", entry, prov, name, prov)
	}
	return fmt.Sprintf("%q: %q is not a registered provider — if it is a SKILL name, a skill is not callable as a tool; its work runs through a real provider (python / bash / a hugr provider). Run tool:providers, then tool:tools(<provider>)", entry, prov)
}

// toolCatalogue returns the full, unfiltered provider→tools registry
// from the calling session's ToolManager (parent agent-tier providers
// + own session-tier providers). Distinct from the model-facing
// Snapshot: no allow-set filter, so authors validate names against
// EVERY real tool, including ones their current skills do not grant.
func (h *SessionSkill) toolCatalogue(ctx context.Context) ([]tool.ProviderCatalogue, error) {
	state, ok := extension.SessionStateFromContext(ctx)
	if !ok || state == nil {
		return nil, errNoToolCatalogue
	}
	tm := state.Tools()
	if tm == nil {
		return nil, errNoToolCatalogue
	}
	return tm.Catalogue(ctx)
}

// workspaceDir returns the absolute session workspace directory the
// bundle_dir must live under, or "" when no workspace extension is
// wired (test fixtures). Sub-agents of a mission share one dir, so an
// assembler worker and the registrar that saves its bundle resolve to
// the same root.
func workspaceDir(ctx context.Context) string {
	state, ok := extension.SessionStateFromContext(ctx)
	if !ok || state == nil {
		return ""
	}
	if ws := wsext.FromState(state); ws != nil {
		return ws.Dir()
	}
	return ""
}

// constrainToWorkspace resolves dir (relative → against the session
// workspace) and returns the cleaned absolute path, rejecting any path
// that escapes the workspace subtree. Existence is NOT checked. label
// prefixes the error messages so each caller (skill:save bundle_dir,
// skill:export dest_dir) reads clearly. When no workspace is wired
// (test fixtures) a relative path is rejected and an absolute one is
// accepted as-is.
func constrainToWorkspace(ctx context.Context, label, dir string) (string, error) {
	ws := workspaceDir(ctx)
	abs := dir
	if !filepath.IsAbs(abs) {
		if ws == "" {
			return "", fmt.Errorf("%w: %s %q must be an absolute path (no workspace is wired to resolve a relative one)", tool.ErrArgValidation, label, dir)
		}
		abs = filepath.Join(ws, dir)
	}
	abs = filepath.Clean(abs)
	if ws != "" {
		rel, err := filepath.Rel(ws, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("%w: %s %q escapes the session workspace %q — stay inside your workspace directory", tool.ErrPathEscape, label, abs, ws)
		}
	}
	return abs, nil
}

// resolveBundleDir validates the caller-supplied bundle_dir: it must
// resolve inside the session workspace AND be an existing directory.
func resolveBundleDir(ctx context.Context, dir string) (string, error) {
	abs, err := constrainToWorkspace(ctx, "skill:save: bundle_dir", dir)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("%w: skill:save: bundle_dir %q does not exist — create it and write SKILL.md into it first", tool.ErrNotFound, abs)
		}
		return "", fmt.Errorf("%w: skill:save: stat bundle_dir %q: %v", tool.ErrIO, abs, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%w: skill:save: bundle_dir %q is not a directory", tool.ErrArgValidation, abs)
	}
	return abs, nil
}

// materializeSkill writes a resolved skill's bundle into destAbs (an
// existing directory) so the agent can edit the files and re-save with
// overwrite. Copies the on-disk bundle verbatim when the skill has an
// FS (system / local / dynamic); reconstructs SKILL.md from the parsed
// manifest for fs-less skills (inline / hub). Returns the sorted list
// of written bundle-relative paths.
func materializeSkill(sk skillpkg.Skill, destAbs string) ([]string, error) {
	var files []string
	wroteManifest := false
	if sk.FS != nil {
		if err := fs.WalkDir(sk.FS, ".", func(p string, d fs.DirEntry, werr error) error {
			if werr != nil {
				return werr
			}
			if p == "." || d.IsDir() {
				return nil
			}
			data, rerr := fs.ReadFile(sk.FS, p)
			if rerr != nil {
				return rerr
			}
			target := filepath.Join(destAbs, filepath.FromSlash(p))
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(target, data, 0o644); err != nil {
				return err
			}
			files = append(files, p)
			if p == "SKILL.md" {
				wroteManifest = true
			}
			return nil
		}); err != nil {
			return nil, fmt.Errorf("%w: skill:export: copy bundle: %v", tool.ErrIO, err)
		}
	}
	if !wroteManifest {
		// fs-less skill (inline / hub): reconstruct SKILL.md through the
		// SAME serializer Publish uses, so export→edit→save round-trips
		// byte-identically (no false skill-drift).
		if err := os.WriteFile(filepath.Join(destAbs, "SKILL.md"), skillpkg.EncodeManifest(sk.Manifest), 0o644); err != nil {
			return nil, fmt.Errorf("%w: skill:export: write SKILL.md: %v", tool.ErrIO, err)
		}
		files = append(files, "SKILL.md")
	}
	sort.Strings(files)
	return files, nil
}

// readBundleBody reads the canonical bundle subdirs (references/,
// scripts/, assets/) under bundleDir into an in-memory fs for Publish,
// applying the same per-file path safety as the legacy inline-save
// path. Files OUTSIDE those three subdirs (SKILL.md itself, scratch
// research files the author left in the directory) are excluded so the
// published bundle stays clean. The returned slice is the sorted set
// of bundle-relative keys, used to populate the save verdict.
func readBundleBody(bundleDir string) (fstest.MapFS, []string, error) {
	bundle := fstest.MapFS{}
	var files []string
	for _, cat := range []string{"references", "scripts", "assets"} {
		catDir := filepath.Join(bundleDir, cat)
		info, err := os.Stat(catDir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, nil, fmt.Errorf("%w: skill:save: stat %s: %v", tool.ErrIO, catDir, err)
		}
		if !info.IsDir() {
			continue
		}
		walkErr := filepath.WalkDir(catDir, func(p string, d fs.DirEntry, werr error) error {
			if werr != nil {
				return werr
			}
			if d.IsDir() {
				return nil
			}
			rel, rerr := filepath.Rel(catDir, p)
			if rerr != nil {
				return rerr
			}
			cleaned, cerr := skillpkg.CleanRelPath(filepath.ToSlash(rel))
			if cerr != nil {
				// Hard-fail rather than silently drop the file: a bundle
				// file the author put under a hidden segment (or any
				// non-normalised path) would otherwise be excluded from
				// the published skill with no signal, and the saved
				// skill's body would reference a file that isn't there.
				// WalkDir never yields a `..` from inside the bundle, so
				// the only rejections here are hidden / dotted segments.
				return fmt.Errorf("%w: skill:save: bundle file %s/%s rejected — remove it or rename (no leading /, no .., no hidden/dot segments)", skillpkg.ErrInvalidPath, cat, filepath.ToSlash(rel))
			}
			data, derr := os.ReadFile(p)
			if derr != nil {
				return fmt.Errorf("read %s: %w", p, derr)
			}
			key := cat + "/" + cleaned
			bundle[key] = &fstest.MapFile{Data: data}
			files = append(files, key)
			return nil
		})
		if walkErr != nil {
			return nil, nil, fmt.Errorf("skill:save: %w", walkErr)
		}
	}
	sort.Strings(files)
	return bundle, files, nil
}
