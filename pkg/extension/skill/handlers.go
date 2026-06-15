package skill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/extension"
	skillpkg "github.com/hugr-lab/hugen/pkg/skill"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// Permission objects keep the same hugen:tool:system grouping the
// SystemProvider used so existing operator floors
// (`hugen:tool:system` disabled across the board) keep applying
// without rewrites. skill_files additionally consults
// hugen:command:skill_files:<skill> for fine-grained gating.
const (
	permObjectLoad          = "hugen:tool:system"
	permObjectUnload        = "hugen:tool:system"
	permObjectSave          = "hugen:tool:system"
	permObjectUninstall     = "hugen:tool:system"
	permObjectFiles         = "hugen:tool:system"
	permObjectRef           = "hugen:tool:system"
	permObjectFilesPerSkill = "hugen:command:skill_files"
)

// skillFilesMaxEntries caps the listing per the contract (SC-010).
// Beyond this, the result envelope sets truncated=true so the model
// narrows with subdir / glob and re-calls.
const skillFilesMaxEntries = 1000

const (
	loadSchema = `{
  "type": "object",
  "properties": {
    "name": {"type": "string", "description": "Skill name as listed in the catalogue (e.g. \"hugr-data\")."}
  },
  "required": ["name"]
}`

	unloadSchema = `{
  "type": "object",
  "properties": {
    "name": {"type": "string", "description": "Skill name to unload."}
  },
  "required": ["name"]
}`

	uninstallSchema = `{
  "type": "object",
  "properties": {
    "name": {"type": "string", "description": "Skill name to remove from the store entirely (bundle + index row). Destructive and approval-gated. Distinct from skill:unload, which only drops it from the current session. To reinstall / update a skill instead of deleting it, use skill:save with overwrite=true."}
  },
  "required": ["name"]
}`

	// saveSchema is path-based: the bundle already lives as files in
	// the session workspace (SKILL.md + references/ + scripts/ +
	// assets/), so the model passes a directory rather than inlining
	// every file through a handoff. Validate-then-register is atomic.
	saveSchema = `{
  "type": "object",
  "properties": {
    "bundle_dir": {
      "type": "string",
      "description": "Absolute path (or path relative to your session workspace) of the bundle directory. It MUST contain SKILL.md; optional references/, scripts/, assets/ subdirs are read recursively. Files outside those three subdirs are ignored. The bundle_dir must live inside your session workspace."
    },
    "validate_only": {
      "type": "boolean",
      "description": "Default false. When true, runs the full validation (manifest parse + task-block placement + allowed_tools_default name check) and returns the verdict WITHOUT registering — a cheap pre-commit check. Use it to confirm a bundle is correct before the real save."
    },
    "overwrite": {
      "type": "boolean",
      "description": "Default false — a name collision returns ErrSkillExists; ask the user before retrying with overwrite=true (this reinstalls / updates the existing skill). Within a post-save validation iteration loop the agent may set this without asking."
    }
  },
  "required": ["bundle_dir"]
}`

	filesSchema = `{
  "type": "object",
  "properties": {
    "name":   {"type": "string", "description": "Loaded skill name."},
    "subdir": {"type": "string", "description": "Optional sub-directory under the skill root (e.g. \"references\")."},
    "glob":   {"type": "string", "description": "Optional filepath.Match-flavour glob filter on the relative path (e.g. \"*.md\")."}
  },
  "required": ["name"]
}`

	refSchema = `{
  "type": "object",
  "properties": {
    "skill": {"type": "string", "description": "Loaded skill name."},
    "ref":   {"type": "string", "description": "Reference base name without the .md extension (e.g. \"instructions\")."}
  },
  "required": ["skill", "ref"]
}`
)

// List implements [tool.ToolProvider].
func (e *Extension) List(_ context.Context) ([]tool.Tool, error) {
	return []tool.Tool{
		{
			Name:             providerName + ":load",
			Description:      "Load a skill (and transitive requires) into the caller's session. Use the catalogue from your system prompt to discover available skills.",
			Provider:         providerName,
			PermissionObject: permObjectLoad,
			ArgSchema:        json.RawMessage(loadSchema),
		},
		{
			Name:             providerName + ":unload",
			Description:      "Unload a skill from the caller's session.",
			Provider:         providerName,
			PermissionObject: permObjectUnload,
			ArgSchema:        json.RawMessage(unloadSchema),
		},
		{
			Name:             providerName + ":uninstall",
			Description:      "Remove a skill from the store entirely (on-disk bundle + index row) — the explicit deletion path. Destructive and approval-gated. To UPDATE a skill, prefer skill:save with overwrite=true; uninstall is for retiring a skill outright.",
			Provider:         providerName,
			PermissionObject: permObjectUninstall,
			ArgSchema:        json.RawMessage(uninstallSchema),
			RequiresApproval: true,
		},
		{
			Name:             providerName + ":save",
			Description:      "Validate and register a skill bundle from a directory in your workspace (SKILL.md + optional references / scripts / assets). Validation (manifest parse + task-block placement + allowed_tools_default name check) runs BEFORE any write; on success the skill is registered and auto-loaded in the current session. Pass validate_only=true for a dry-run verdict. User-initiated only — do NOT propose this. The authoring format + flow is owned by the `_skill_builder` skill.",
			Provider:         providerName,
			PermissionObject: permObjectSave,
			ArgSchema:        json.RawMessage(saveSchema),
		},
		{
			Name:             providerName + ":files",
			Description:      "List on-disk files of a loaded skill with relative + absolute paths so other tools (bash.read_file, python.run_script) can read them. Optional subdir narrows the listing; optional glob filters by path pattern.",
			Provider:         providerName,
			PermissionObject: permObjectFiles,
			ArgSchema:        json.RawMessage(filesSchema),
		},
		{
			Name:             providerName + ":ref",
			Description:      "Read a reference document (references/<ref>.md) from a loaded skill. References are listed in the skill's SKILL.md body.",
			Provider:         providerName,
			PermissionObject: permObjectRef,
			ArgSchema:        json.RawMessage(refSchema),
		},
		{
			Name:             toolNameCatalogList,
			Description:      toolDescCatalogList,
			Provider:         providerName,
			PermissionObject: permObjectCatalogList,
			ArgSchema:        json.RawMessage(catalogListSchema),
		},
	}, nil
}

// Call implements [tool.ToolProvider]. Routes by short tool name
// after stripping the "skill:" prefix.
func (e *Extension) Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	short := name
	if pfx := providerName + ":"; strings.HasPrefix(name, pfx) {
		short = name[len(pfx):]
	}
	state, ok := extension.SessionStateFromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("%w: skill: no session attached to dispatch ctx", tool.ErrSystemUnavailable)
	}
	h := FromState(state)
	if h == nil {
		return nil, fmt.Errorf("%w: skill: extension state not initialised", tool.ErrSystemUnavailable)
	}
	switch short {
	case "load":
		return h.callLoad(ctx, args)
	case "unload":
		return h.callUnload(ctx, args)
	case "uninstall":
		return h.callUninstall(ctx, args)
	case "save":
		return h.callSave(ctx, args)
	case "files":
		return h.callFiles(ctx, args)
	case "ref":
		return h.callRef(ctx, args)
	case "catalog_list":
		return h.callCatalogList(ctx, args)
	default:
		return nil, fmt.Errorf("%w: skill:%s", tool.ErrUnknownTool, short)
	}
}

// Subscribe implements [tool.ToolProvider]. The catalogue is static.
func (e *Extension) Subscribe(_ context.Context) (<-chan tool.ProviderEvent, error) {
	return nil, nil
}

// Close implements [tool.ToolProvider]. Per-session state cleanup
// flows through the separate [extension.Closer.CloseSession] hook
// (recovery.go); the provider itself holds no resources.
func (e *Extension) Close() error { return nil }

// allowedSkillsFromState pulls the spawner-scoped whitelist out of
// the session state, or returns (nil, false) when the spawner did
// not narrow the surface. A present-but-empty list still returns
// (allowed, true) — that's the "lock the session to its pre-loaded
// surface" mode (task ext default for recipes that declare no
// extra `allowed_skills`). Phase 6.1d.
func allowedSkillsFromState(state extension.SessionState) ([]string, bool) {
	if state == nil {
		return nil, false
	}
	v, ok := state.Value(SessionAllowedSkillsKey)
	if !ok {
		return nil, false
	}
	switch t := v.(type) {
	case []string:
		return t, true
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out, true
	default:
		return nil, false
	}
}

// allowSkillLoad reports whether the given skill name is loadable
// in the current session. Sessions without a spawner-scoped
// whitelist (the absent-key default) always allow. Sessions with a
// whitelist allow:
//
//   - Universal baseline (`_system`, `_worker`) — autoloaded for
//     every worker-tier session regardless of override.
//   - The spawner's [SessionAllowedSkillsKey] whitelist entries.
//   - Skills already loaded in the session — repeated `skill:load`
//     of a loaded skill is a no-op the manager handles gracefully,
//     so blocking it would create a false-negative for harmless
//     model behaviour.
//
// Transitive `requires_skills` of an allowed name still load via
// SkillManager.Load's closure walker; the gate runs only against
// the top-level name the LLM passed. Phase 6.1d.
func allowSkillLoad(ctx context.Context, h *SessionSkill, state extension.SessionState, name string) bool {
	allowed, scoped := allowedSkillsFromState(state)
	if !scoped {
		return true
	}
	switch name {
	case "_system", "_worker":
		return true
	}
	// Already-loaded — let the no-op reach Load() so the LLM can't
	// trip on a repeat call. Cheap lookup against the live binding
	// set (no manifest walk).
	if h != nil {
		for _, n := range h.LoadedNames(ctx) {
			if n == name {
				return true
			}
		}
	}
	for _, entry := range allowed {
		if entry == name {
			return true
		}
	}
	return false
}

// ---------- skill:load ----------

func (h *SessionSkill) callLoad(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if h.manager == nil {
		return nil, tool.ErrSystemUnavailable
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("%w: skill:load: %v", tool.ErrArgValidation, err)
	}
	if in.Name == "" {
		return nil, fmt.Errorf("%w: skill:load: name required", tool.ErrArgValidation)
	}
	// Phase 6.1d — spawner-scoped whitelist (additive on top of the
	// tier autoload baseline). When the calling session was opened
	// with a [SessionAllowedSkillsKey] value (today: task ext recipe
	// children), `skill:load` is restricted to: the universal
	// `_system` / `_worker` baseline, the whitelist entries, and any
	// already-loaded skill (harmless repeat). Anything else returns
	// a structured envelope the LLM can react to without retrying
	// the same name.
	if state, ok := extension.SessionStateFromContext(ctx); ok {
		if !allowSkillLoad(ctx, h, state, in.Name) {
			body, mErr := json.Marshal(map[string]any{
				"error": map[string]string{
					"code":    "skill_not_in_allowlist",
					"message": fmt.Sprintf("skill:load %q denied: this session was opened with a scoped allow-list and %q is not in it. Execute the loaded recipe's steps using the skills already in your catalogue.", in.Name, in.Name),
				},
			})
			if mErr != nil {
				return nil, fmt.Errorf("%w: skill:load: marshal allowlist denial: %v", tool.ErrSystemUnavailable, mErr)
			}
			return body, nil
		}
	}
	if err := h.Load(ctx, in.Name); err != nil {
		// Tier mismatch is the only error the LLM can productively
		// recover from — surface it as a structured envelope so the
		// model sees code + hint without parsing the Go error
		// string. Other errors (cycle, not_found, perm) keep the
		// existing native-error path.
		if errors.Is(err, skillpkg.ErrTierForbidden) {
			body, mErr := json.Marshal(map[string]any{
				"error": map[string]string{
					"code":    "tier_forbidden",
					"message": err.Error(),
				},
			})
			if mErr != nil {
				return nil, err
			}
			return body, nil
		}
		return nil, err
	}
	h.emitOp(ctx, OpLoad, in.Name)
	return json.RawMessage(`{"loaded":true}`), nil
}

// ---------- skill:unload ----------

func (h *SessionSkill) callUnload(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if h.manager == nil {
		return nil, tool.ErrSystemUnavailable
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("%w: skill:unload: %v", tool.ErrArgValidation, err)
	}
	if err := h.Unload(ctx, in.Name); err != nil {
		return nil, err
	}
	h.emitOp(ctx, OpUnload, in.Name)
	return json.RawMessage(`{"unloaded":true}`), nil
}

// ---------- skill:uninstall ----------

// callUninstall removes a skill from the store entirely (bundle +
// index row), the deletion counterpart to skill:save. Destructive and
// approval-gated at the dispatcher (RequiresApproval). If the skill
// was loaded in the calling session it is unloaded first so the
// session view stays consistent. Returns ErrUnsupportedBackend
// (surfaced as a structured envelope) when the store has no removable
// backend — e.g. a local-only store where the skill lives in a
// read-through dir; in that case overwrite-save is the update path.
func (h *SessionSkill) callUninstall(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if h.manager == nil {
		return nil, tool.ErrSystemUnavailable
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("%w: skill:uninstall: %v", tool.ErrArgValidation, err)
	}
	if strings.TrimSpace(in.Name) == "" {
		return nil, fmt.Errorf("%w: skill:uninstall: name required", tool.ErrArgValidation)
	}
	// Remove from the store first — the authoritative, fallible action.
	// Only after it succeeds do we drop the skill from the live session,
	// so a failed removal leaves no partial (unloaded-but-present) state.
	if err := h.manager.Uninstall(ctx, in.Name); err != nil {
		if errors.Is(err, skillpkg.ErrUnsupportedBackend) {
			body, mErr := json.Marshal(map[string]any{
				"error": map[string]string{
					"code":    "skill_uninstall_unsupported",
					"message": fmt.Sprintf("skill:uninstall %q: the store has no removable backend (the skill lives in a read-through source). To update it, re-run skill:save with overwrite=true instead.", in.Name),
				},
			})
			if mErr != nil {
				return nil, err
			}
			return body, nil
		}
		return nil, fmt.Errorf("skill:uninstall: %w", err)
	}
	// Drop it from this session so the model's live catalogue no longer
	// offers a skill that is gone from the store (best-effort).
	for _, n := range h.LoadedNames(ctx) {
		if n == in.Name {
			if err := h.Unload(ctx, in.Name); err == nil {
				h.emitOp(ctx, OpUnload, in.Name)
			}
			break
		}
	}
	return json.Marshal(map[string]any{"uninstalled": in.Name})
}

// ---------- skill:save ----------

// saveInput is the parsed argument shape; mirrors saveSchema.
type saveInput struct {
	BundleDir    string `json:"bundle_dir"`
	Overwrite    bool   `json:"overwrite,omitempty"`
	ValidateOnly bool   `json:"validate_only,omitempty"`
}

// saveResult is the JSON envelope returned to the LLM after a
// successful save (or a validate_only verdict). The model uses Files
// to drive its mandatory post-save validation (run scripts/* against
// test parameters); Directory is the on-disk root the saved-skill
// body's ${SKILL_DIR}/... references resolve against. ValidateOnly +
// Valid distinguish a dry-run verdict from a real registration.
type saveResult struct {
	Name         string   `json:"name"`
	Directory    string   `json:"directory,omitempty"`
	Files        []string `json:"files"`
	ValidateOnly bool     `json:"validate_only,omitempty"`
	Valid        bool     `json:"valid,omitempty"`
}

// callSave reads a skill bundle from a workspace directory, validates
// it (manifest parse + task-block placement + allowed_tools_default
// name check), then — unless validate_only — registers it to the
// local store and auto-loads it in the current session. Validation is
// atomic with the write: nothing is published until every check
// passes. See design/005-reuse-and-memory/spec-skill-authoring.md D1.
//
// Error mapping (errors.Is-checkable for the LLM consumer via the
// runtime's tool-error envelope):
//   - tool.ErrArgValidation        — malformed args / missing bundle_dir.
//   - tool.ErrNotFound             — bundle_dir or SKILL.md absent.
//   - tool.ErrPathEscape           — bundle_dir escapes the workspace.
//   - skillpkg.ErrManifestInvalid  — SKILL.md fails Parse.
//   - skillpkg.ErrAutoloadReserved — manifest sets autoload:true.
//   - skillpkg.ErrTaskBlockMisplaced — task block mis-nested.
//   - ErrUnknownToolName           — allowed_tools_default names a
//     tool absent from the registry.
//   - skillpkg.ErrInvalidPath      — bundle file escapes safety.
//   - skillpkg.ErrSkillExists      — name collision and !overwrite.
//   - tool.ErrSystemUnavailable    — manager not wired.
func (h *SessionSkill) callSave(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if h.manager == nil {
		return nil, tool.ErrSystemUnavailable
	}
	var in saveInput
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("%w: skill:save: %v", tool.ErrArgValidation, err)
	}
	if strings.TrimSpace(in.BundleDir) == "" {
		return nil, fmt.Errorf("%w: skill:save: bundle_dir is required (path to the bundle directory in your workspace containing SKILL.md)", tool.ErrArgValidation)
	}

	bundleDir, err := resolveBundleDir(ctx, in.BundleDir)
	if err != nil {
		return nil, err
	}

	mdPath := filepath.Join(bundleDir, "SKILL.md")
	rawMD, err := os.ReadFile(mdPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: skill:save: no SKILL.md in %s — write the manifest to <bundle_dir>/SKILL.md first", tool.ErrNotFound, bundleDir)
		}
		return nil, fmt.Errorf("%w: skill:save: read %s: %v", tool.ErrIO, mdPath, err)
	}
	manifest, err := skillpkg.Parse(rawMD)
	if err != nil {
		return nil, fmt.Errorf("skill:save: SKILL.md does not parse — fix the frontmatter and re-save: %w", err)
	}

	// Full validation — identical verdict for dry-run and real save.
	if err := h.validateAuthoring(ctx, manifest); err != nil {
		return nil, err
	}

	bundle, files, err := readBundleBody(bundleDir)
	if err != nil {
		return nil, err
	}

	if in.ValidateOnly {
		return json.Marshal(saveResult{
			Name:         manifest.Name,
			Directory:    bundleDir,
			Files:        files,
			ValidateOnly: true,
			Valid:        true,
		})
	}

	if err := h.manager.Publish(ctx, manifest, bundle, skillpkg.PublishOptions{Overwrite: in.Overwrite}); err != nil {
		if errors.Is(err, skillpkg.ErrSkillExists) {
			// Action-oriented message — both gemma and claude
			// rationalised the prior generic "io: already
			// exists" envelope as "no-op" or "success". The
			// explicit hint about asking the user + overwrite
			// flag makes the recovery path obvious.
			return nil, fmt.Errorf("skill:save: %w — skill %q is already in the local store; ASK THE USER before retrying with `overwrite: true`, OR pick a different name. Do NOT silently retry", skillpkg.ErrSkillExists, manifest.Name)
		}
		return nil, fmt.Errorf("skill:save: %w", err)
	}

	// Auto-load the freshly-saved skill so the model can use it
	// immediately in this session and run the validation loop
	// against bundled scripts. If auto-load fails (most likely:
	// requires_skills resolves a missing dependency), the skill
	// is already on disk — surface the partial-success path with
	// a clear hint instead of leaving the model to wonder. The
	// model's recovery: tell the user, suggest manual `/skill
	// load <name>` after fixing the dependency.
	if err := h.Load(ctx, manifest.Name); err != nil {
		return nil, fmt.Errorf("skill:save: skill %q saved to local store but auto-load in this session failed (likely a missing requires_skills dependency); you can `/skill load %s` after resolving the dependency: %w",
			manifest.Name, manifest.Name, err)
	}

	loaded, err := h.LoadedSkill(ctx, manifest.Name)
	if err != nil {
		return nil, fmt.Errorf("skill:save: skill %q saved and loaded but lookup for the result envelope failed: %w", manifest.Name, err)
	}

	// Re-derive the file list from what actually landed on disk (the
	// post-load walk includes SKILL.md, unlike the bundle-body list).
	files = files[:0]
	if loaded.FS != nil {
		_ = fs.WalkDir(loaded.FS, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || p == "." {
				return nil
			}
			files = append(files, p)
			return nil
		})
		sort.Strings(files)
	}

	return json.Marshal(saveResult{
		Name:      manifest.Name,
		Directory: loaded.Root,
		Files:     files,
		Valid:     true,
	})
}

// ---------- skill:ref ----------

func (h *SessionSkill) callRef(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if h.manager == nil {
		return nil, tool.ErrSystemUnavailable
	}
	var in struct {
		Skill string `json:"skill"`
		Ref   string `json:"ref"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("%w: skill:ref: %v", tool.ErrArgValidation, err)
	}
	if in.Skill == "" || in.Ref == "" {
		return nil, fmt.Errorf("%w: skill:ref: skill and ref required", tool.ErrArgValidation)
	}
	loaded, err := h.LoadedSkill(ctx, in.Skill)
	if err != nil {
		return nil, fmt.Errorf("skill:ref: %w", err)
	}
	if loaded.FS == nil {
		return nil, fmt.Errorf("skill:ref: %s has no body fs", in.Skill)
	}
	// References are addressed by base name (e.g. "instructions"); the
	// file on disk has a .md extension. Try the as-supplied path first
	// so callers that already passed an explicit extension (or
	// sub-directory) keep working.
	refPath := "references/" + in.Ref
	body, err := readSkillFile(loaded.FS, refPath)
	if err != nil {
		altPath := refPath + ".md"
		if alt, altErr := readSkillFile(loaded.FS, altPath); altErr == nil {
			body, err = alt, nil
			refPath = altPath
		}
	}
	_ = refPath
	if err != nil {
		return nil, fmt.Errorf("skill:ref: %s/%s: %w", in.Skill, in.Ref, err)
	}
	return json.Marshal(map[string]string{"skill": in.Skill, "ref": in.Ref, "body": string(body)})
}

// ---------- skill:files ----------

type fileEntry struct {
	Rel  string `json:"rel"`
	Abs  string `json:"abs"`
	Size int64  `json:"size"`
	Mode string `json:"mode"`
}

type filesResult struct {
	Skill     string      `json:"skill"`
	Root      string      `json:"root"`
	Files     []fileEntry `json:"files"`
	Truncated bool        `json:"truncated,omitempty"`
}

func (h *SessionSkill) callFiles(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if h.manager == nil {
		return nil, tool.ErrSystemUnavailable
	}
	var in struct {
		Name   string `json:"name"`
		Subdir string `json:"subdir,omitempty"`
		Glob   string `json:"glob,omitempty"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("%w: skill:files: %v", tool.ErrArgValidation, err)
	}
	if in.Name == "" {
		return nil, fmt.Errorf("%w: skill:files: name required", tool.ErrArgValidation)
	}
	if in.Glob != "" {
		// fail-fast on malformed pattern.
		if _, err := filepath.Match(in.Glob, ""); err != nil {
			return nil, fmt.Errorf("%w: skill:files: bad glob %q: %v", tool.ErrArgValidation, in.Glob, err)
		}
	}
	if err := gateFiles(ctx, h.perms, in.Name); err != nil {
		return nil, err
	}
	loaded, err := h.LoadedSkill(ctx, in.Name)
	if err != nil {
		if errors.Is(err, skillpkg.ErrSkillNotFound) {
			return nil, fmt.Errorf("%w: skill not loaded: %s", tool.ErrNotFound, in.Name)
		}
		return nil, fmt.Errorf("skill:files: %w", err)
	}
	out := filesResult{Skill: in.Name, Files: []fileEntry{}}
	if loaded.Root == "" {
		// Inline / hub skill — no on-disk content to surface.
		return json.Marshal(out)
	}
	out.Root = loaded.Root
	walkStart := loaded.Root
	if in.Subdir != "" {
		if filepath.IsAbs(in.Subdir) {
			return nil, fmt.Errorf("%w: skill:files: subdir must be relative", tool.ErrPathEscape)
		}
		joined := filepath.Join(loaded.Root, in.Subdir)
		rel, err := filepath.Rel(loaded.Root, joined)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("%w: skill:files: subdir escapes skill root", tool.ErrPathEscape)
		}
		walkStart = joined
	}
	walkErr := filepath.WalkDir(walkStart, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			if errors.Is(werr, fs.ErrNotExist) {
				return fs.SkipAll
			}
			return werr
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(loaded.Root, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if in.Glob != "" {
			ok, _ := filepath.Match(in.Glob, rel)
			if !ok {
				return nil
			}
		}
		info, statErr := d.Info()
		if statErr != nil {
			return nil
		}
		if len(out.Files) >= skillFilesMaxEntries {
			out.Truncated = true
			return fs.SkipAll
		}
		out.Files = append(out.Files, fileEntry{
			Rel:  rel,
			Abs:  path,
			Size: info.Size(),
			Mode: fmt.Sprintf("0%o", info.Mode().Perm()),
		})
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, fs.SkipAll) {
		return nil, fmt.Errorf("%w: skill:files: walk %s: %v", tool.ErrIO, walkStart, walkErr)
	}
	sort.Slice(out.Files, func(i, j int) bool { return out.Files[i].Rel < out.Files[j].Rel })
	return json.Marshal(out)
}

// gateFiles consults Tier-1 / Tier-2 for hugen:command:skill_files:
// <name>. Default decision is allow (the listing is informational);
// operators may pin a deny rule for sensitive skills.
func gateFiles(ctx context.Context, perms perm.Service, name string) error {
	if perms == nil {
		return nil
	}
	got, err := perms.Resolve(ctx, permObjectFilesPerSkill, name)
	if err != nil {
		return err
	}
	if got.Disabled {
		return fmt.Errorf("%w: skill:files(%s) denied by %s tier",
			tool.ErrPermissionDenied, name, deniedFilesTier(got))
	}
	return nil
}

func deniedFilesTier(p perm.Permission) string {
	switch {
	case p.FromConfig && p.FromRemote:
		return "config+remote"
	case p.FromRemote:
		return "remote"
	case p.FromConfig:
		return "config"
	default:
		return "unknown"
	}
}

func readSkillFile(fsys fs.FS, name string) ([]byte, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}
