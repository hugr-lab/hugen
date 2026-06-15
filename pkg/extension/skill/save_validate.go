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
// before publish — the same verdict for `validate_only` dry-runs and
// real saves. Three layers:
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
		problems = append(problems, h.toolNameHint(ctx, entry, providers))
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
// unknown the classic cause is naming a SKILL as a provider — detected
// via the skill manager so the message names the exact mistake.
func (h *SessionSkill) toolNameHint(ctx context.Context, entry string, providers map[string]struct{}) string {
	prov, name := entry, ""
	if i := strings.IndexByte(entry, ':'); i > 0 {
		prov, name = entry[:i], entry[i+1:]
	}
	if _, ok := providers[prov]; ok {
		return fmt.Sprintf("%q: provider %q has no tool %q — run tool:tools(%q) for its real tool names", entry, prov, name, prov)
	}
	if h.isKnownSkill(ctx, prov) {
		return fmt.Sprintf("%q: %q is a SKILL, not a provider — a skill is not callable as a tool; the work it does runs through a real provider (e.g. python/bash/a hugr provider). Run tool:providers, then tool:tools(<provider>)", entry, prov)
	}
	return fmt.Sprintf("%q: %q is not a registered provider — run tool:providers to list real providers", entry, prov)
}

// isKnownSkill reports whether name resolves to a skill in the store —
// used only to sharpen the unknown-provider hint, never to gate.
func (h *SessionSkill) isKnownSkill(ctx context.Context, name string) bool {
	if h.manager == nil || name == "" {
		return false
	}
	_, err := h.manager.Get(ctx, name)
	return err == nil
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

// resolveBundleDir validates the caller-supplied bundle_dir: relative
// paths resolve against the session workspace; the result is
// constrained to the workspace subtree (path-escape defence) and must
// be an existing directory. When no workspace is wired (fixture) the
// cleaned absolute path is accepted as-is after the stat check.
func resolveBundleDir(ctx context.Context, dir string) (string, error) {
	ws := workspaceDir(ctx)
	abs := dir
	if !filepath.IsAbs(abs) {
		if ws == "" {
			return "", fmt.Errorf("%w: skill:save: bundle_dir must be an absolute path (no workspace is wired to resolve a relative one)", tool.ErrArgValidation)
		}
		abs = filepath.Join(ws, dir)
	}
	abs = filepath.Clean(abs)
	if ws != "" {
		rel, err := filepath.Rel(ws, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("%w: skill:save: bundle_dir %q escapes the session workspace %q — build the bundle inside your workspace directory", tool.ErrPathEscape, abs, ws)
		}
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
				// Editor / OS cruft (dotfiles, hidden dirs) — skip
				// rather than fail the whole save. WalkDir never yields
				// a `..` segment from inside the constrained bundle dir,
				// so the only rejections here are hidden segments.
				return nil
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
