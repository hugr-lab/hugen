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
	"testing/fstest"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
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
	permObjectValidate      = "hugen:tool:system"
	permObjectSave          = "hugen:tool:system"
	permObjectExport        = "hugen:tool:system"
	permObjectUninstall     = "hugen:tool:system"
	permObjectFiles         = "hugen:tool:system"
	permObjectRef           = "hugen:tool:system"
	permObjectFilesPerSkill = "hugen:command:skill_files"
	// permObjectPublish resolves as (type_name="hugen:skill", field="publish")
	// — the §4 agent-side publish gate, distinct from the broad
	// hugen:tool:system object so it is grantable on its own (a publishing
	// skill / role grants it; no grant, no dispatch).
	permObjectPublish = "hugen:skill"
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

	exportSchema = `{
  "type": "object",
  "properties": {
    "name":     {"type": "string", "description": "Name of the skill to copy out for editing (any registered skill — system, hub, or local)."},
    "dest_dir": {"type": "string", "description": "Optional destination directory under your session workspace (relative or absolute, must stay inside the workspace). Defaults to the skill name. The skill's SKILL.md + references / scripts / assets are written there so you can edit them and re-register with skill:save(bundle_dir, overwrite=true)."}
  },
  "required": ["name"]
}`

	// validateSchema is the dry-run counterpart to saveSchema: same
	// path-based bundle, NO write. Splitting validation out of save
	// gives an author a register-incapable check tool — it CANNOT
	// publish, only report the verdict.
	validateSchema = `{
  "type": "object",
  "properties": {
    "bundle_dir": {
      "type": "string",
      "description": "Absolute path (or path relative to your session workspace) of the bundle directory to validate. It MUST contain SKILL.md; optional references/, scripts/, assets/ subdirs are read recursively. The bundle_dir must live inside your session workspace. Nothing is written — this only reports whether the bundle would register cleanly."
    }
  },
  "required": ["bundle_dir"]
}`

	// saveSchema is path-based: the bundle already lives as files in
	// the session workspace (SKILL.md + references/ + scripts/ +
	// assets/), so the model passes a directory rather than inlining
	// every file through a handoff. skill:save REGISTERS (validation
	// re-runs first, atomically); use skill:validate for a dry run.
	saveSchema = `{
  "type": "object",
  "properties": {
    "bundle_dir": {
      "type": "string",
      "description": "Absolute path (or path relative to your session workspace) of the bundle directory. It MUST contain SKILL.md; optional references/, scripts/, assets/ subdirs are read recursively. Files outside those three subdirs are ignored. The bundle_dir must live inside your session workspace."
    },
    "overwrite": {
      "type": "boolean",
      "description": "Whether to replace an existing skill of the same name. OMIT it (the default) and a name collision pauses to ASK THE USER (overwrite / save under a new name / cancel) — never silently clobbers. Pass true only when the user has authorised replacing the existing skill; pass false to hard-fail a collision without asking."
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

	publishSchema = `{
  "type": "object",
  "properties": {
    "name": {"type": "string", "description": "Name of a registered skill to publish to the hub marketplace so other agents can install it. The whole bundle (SKILL.md + references / scripts / assets) is uploaded; the hub verifies your publish permission, checks the name is not reserved or owned by another publisher, and requires your role to hold any capabilities the skill declares."}
  },
  "required": ["name"]
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
			Name:             providerName + ":export",
			Description:      "Copy an existing skill's bundle (SKILL.md + references / scripts / assets) into a directory in your session workspace so you can EDIT it. This is the start of the update flow: export → edit the files → skill:save(bundle_dir, overwrite=true). Works on any registered skill.",
			Provider:         providerName,
			PermissionObject: permObjectExport,
			ArgSchema:        json.RawMessage(exportSchema),
		},
		{
			Name:             providerName + ":validate",
			Description:      "Dry-run check a skill bundle in your workspace (SKILL.md + optional references / scripts / assets) WITHOUT registering it — manifest parse + task-block placement + allowed_tools_default name check. Returns the verdict only; it cannot publish. Iterate with this until the bundle is clean, then skill:save to register. The authoring format + flow is documented by the skill-authoring skill.",
			Provider:         providerName,
			PermissionObject: permObjectValidate,
			ArgSchema:        json.RawMessage(validateSchema),
		},
		{
			Name:             providerName + ":save",
			Description:      "Register a skill bundle from a directory in your workspace (SKILL.md + optional references / scripts / assets). Validation re-runs atomically BEFORE any write; on success the skill is registered and auto-loaded in the current session. On a name collision, OMITTING overwrite pauses to ask the user (overwrite / new name / cancel) — it never clobbers silently. Validate first with skill:validate. User-initiated only — do NOT propose this. The authoring format + flow is documented by the skill-authoring skill.",
			Provider:         providerName,
			PermissionObject: permObjectSave,
			ArgSchema:        json.RawMessage(saveSchema),
		},
		{
			Name:             providerName + ":publish",
			Description:      "Publish a registered skill's bundle to the hub marketplace so other agents can install it. Uploads the whole bundle (tool code POSTs it — bytes never pass through the conversation). Requires the hugen:skill.publish permission (granted by a publishing skill) AND passes the hub's checks (reserved-name / first-publisher / declared-capabilities). Outward-facing + approval-gated; user-initiated only — do NOT propose it.",
			Provider:         providerName,
			PermissionObject: permObjectPublish,
			ArgSchema:        json.RawMessage(publishSchema),
			RequiresApproval: true,
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
	case "export":
		return h.callExport(ctx, args)
	case "validate":
		return h.callValidate(ctx, args)
	case "save":
		return h.callSave(ctx, args)
	case "publish":
		return h.callPublish(ctx, args)
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

// ---------- skill:export ----------

// exportResult is the JSON envelope returned after a successful
// export. Dir is the absolute workspace directory the bundle was
// written to (pass it back to skill:save as bundle_dir after editing);
// Files is the sorted set of written bundle-relative paths.
type exportResult struct {
	Name  string   `json:"name"`
	Dir   string   `json:"dir"`
	Files []string `json:"files"`
}

// callExport copies a registered skill's bundle into a workspace
// directory so the agent can edit it and re-register with
// skill:save(overwrite=true). The destination is constrained to the
// session workspace (path-escape defence). See
// design/005-reuse-and-memory/spec-skill-authoring.md (update flow).
func (h *SessionSkill) callExport(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if h.manager == nil {
		return nil, tool.ErrSystemUnavailable
	}
	var in struct {
		Name    string `json:"name"`
		DestDir string `json:"dest_dir,omitempty"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("%w: skill:export: %v", tool.ErrArgValidation, err)
	}
	if strings.TrimSpace(in.Name) == "" {
		return nil, fmt.Errorf("%w: skill:export: name required", tool.ErrArgValidation)
	}
	dest := strings.TrimSpace(in.DestDir)
	if dest == "" {
		dest = in.Name
	}
	abs, err := constrainToWorkspace(ctx, "skill:export: dest_dir", dest)
	if err != nil {
		return nil, err
	}
	sk, err := h.manager.Get(ctx, in.Name)
	if err != nil {
		if errors.Is(err, skillpkg.ErrSkillNotFound) {
			return nil, fmt.Errorf("%w: skill:export: skill %q not found in the store", tool.ErrNotFound, in.Name)
		}
		return nil, fmt.Errorf("skill:export: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("%w: skill:export: mkdir %q: %v", tool.ErrIO, abs, err)
	}
	files, err := materializeSkill(sk, abs)
	if err != nil {
		return nil, err
	}
	return json.Marshal(exportResult{Name: sk.Manifest.Name, Dir: abs, Files: files})
}

// ---------- skill:validate / skill:save ----------
//
// The authoring surface is two distinct tools, NOT one tool with a
// `validate_only` flag (B54): a register-INCAPABLE check
// (skill:validate) and a register-ONLY publish (skill:save). The
// capability boundary lives in the toolset — an author granted only
// skill:validate physically cannot register a bundle — instead of in
// prose a weak model can rationalise around. skill:save additionally
// gates name collisions with a runtime inquiry rather than a prose
// "ask the user" hint, so a collision can never silently overwrite.

// saveInput is the parsed argument shape; mirrors saveSchema.
// Overwrite is a pointer so an ABSENT flag (ask the user on collision)
// reads differently from an explicit true (replace) or false (hard
// fail) — the distinction the collision gate turns on.
type saveInput struct {
	BundleDir string `json:"bundle_dir"`
	Overwrite *bool  `json:"overwrite,omitempty"`
}

// validateInput mirrors validateSchema (bundle_dir only).
type validateInput struct {
	BundleDir string `json:"bundle_dir"`
}

// saveResult is the JSON envelope returned after a successful register.
// The model uses Files to drive its mandatory post-save validation
// (run scripts/* against test parameters); Directory is the on-disk
// root the saved-skill body's ${SKILL_DIR}/... references resolve
// against.
type saveResult struct {
	Name      string   `json:"name"`
	Directory string   `json:"directory,omitempty"`
	Files     []string `json:"files"`
	Valid     bool     `json:"valid,omitempty"`
}

// validateResult is the dry-run verdict from skill:validate — same
// shape as a save envelope minus the registration, with ValidateOnly
// set so the model never mistakes a check for a publish.
type validateResult struct {
	Name         string   `json:"name"`
	Directory    string   `json:"directory,omitempty"`
	Files        []string `json:"files"`
	ValidateOnly bool     `json:"validate_only"`
	Valid        bool     `json:"valid"`
}

// loadValidatedBundle reads + parses + fully validates a bundle dir
// (the path shared by skill:validate and skill:save). Returns the
// parsed manifest, its body FS, the relative file list, and the
// resolved absolute dir. Every error is typed + actionable so the
// author fixes the files and re-calls. `label` prefixes errors with
// the calling tool name.
func (h *SessionSkill) loadValidatedBundle(ctx context.Context, label, bundleDirArg string) (skillpkg.Manifest, fstest.MapFS, []string, string, error) {
	if strings.TrimSpace(bundleDirArg) == "" {
		return skillpkg.Manifest{}, nil, nil, "", fmt.Errorf("%w: %s: bundle_dir is required (path to the bundle directory in your workspace containing SKILL.md)", tool.ErrArgValidation, label)
	}
	bundleDir, err := resolveBundleDir(ctx, bundleDirArg)
	if err != nil {
		return skillpkg.Manifest{}, nil, nil, "", err
	}
	mdPath := filepath.Join(bundleDir, "SKILL.md")
	rawMD, err := os.ReadFile(mdPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return skillpkg.Manifest{}, nil, nil, "", fmt.Errorf("%w: %s: no SKILL.md in %s — write the manifest to <bundle_dir>/SKILL.md first", tool.ErrNotFound, label, bundleDir)
		}
		return skillpkg.Manifest{}, nil, nil, "", fmt.Errorf("%w: %s: read %s: %v", tool.ErrIO, label, mdPath, err)
	}
	manifest, err := skillpkg.Parse(rawMD)
	if err != nil {
		return skillpkg.Manifest{}, nil, nil, "", fmt.Errorf("%s: SKILL.md does not parse — fix the frontmatter and re-validate: %w", label, err)
	}
	if err := h.validateAuthoring(ctx, manifest); err != nil {
		return skillpkg.Manifest{}, nil, nil, "", err
	}
	bundle, files, err := readBundleBody(bundleDir)
	if err != nil {
		return skillpkg.Manifest{}, nil, nil, "", err
	}
	return manifest, bundle, files, bundleDir, nil
}

// callValidate runs every save-time check over a bundle and returns
// the verdict WITHOUT registering. It cannot publish — the
// register-incapable half of the authoring split (B54).
func (h *SessionSkill) callValidate(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if h.manager == nil {
		return nil, tool.ErrSystemUnavailable
	}
	var in validateInput
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("%w: skill:validate: %v", tool.ErrArgValidation, err)
	}
	manifest, _, files, bundleDir, err := h.loadValidatedBundle(ctx, "skill:validate", in.BundleDir)
	if err != nil {
		return nil, err
	}
	return json.Marshal(validateResult{
		Name:         manifest.Name,
		Directory:    bundleDir,
		Files:        files,
		ValidateOnly: true,
		Valid:        true,
	})
}

// callSave reads a skill bundle from a workspace directory, re-runs
// the full validation atomically, then REGISTERS it to the local
// store and auto-loads it in the current session. Nothing is
// published until every check passes. On a name collision the
// `overwrite` flag decides: absent → the runtime ASKS the user
// (overwrite / new name / cancel) so a collision never silently
// clobbers; explicit true → replace; explicit false → hard fail. See
// design/005-reuse-and-memory/spec-skill-authoring.md D1 + backlog B54.
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
//   - skillpkg.ErrSkillExists      — name collision the user declined
//     to overwrite (or explicit overwrite:false).
//   - errSaveCancelled             — user cancelled the collision modal.
//   - tool.ErrSystemUnavailable    — manager not wired.
func (h *SessionSkill) callSave(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if h.manager == nil {
		return nil, tool.ErrSystemUnavailable
	}
	var in saveInput
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("%w: skill:save: %v", tool.ErrArgValidation, err)
	}

	// skill:save ALWAYS re-validates before it writes — the same full
	// check skill:validate runs. It is structurally impossible to
	// register an unvalidated or invalid bundle: validation is inline
	// here, not a separate step the model can skip. On a validation
	// failure, redirect to skill:validate so the model fixes the files
	// and confirms green before retrying the save (user request: "save
	// must re-validate; on error, validate first").
	manifest, bundle, files, _, err := h.loadValidatedBundle(ctx, "skill:save", in.BundleDir)
	if err != nil {
		if isAuthoringValidationError(err) {
			return nil, fmt.Errorf("%w\nThe bundle did NOT register — skill:save re-runs this exact check before every write and refuses to persist an invalid bundle. Fix the files, run skill:validate until it returns valid:true, then skill:save", err)
		}
		return nil, err
	}

	// Register-only. On collision the gate decides whether to retry
	// with Overwrite=true (the user agreed) or surface a typed error.
	ow := in.Overwrite != nil && *in.Overwrite
	pubErr := h.manager.Publish(ctx, manifest, bundle, skillpkg.PublishOptions{Overwrite: ow})
	if errors.Is(pubErr, skillpkg.ErrSkillExists) {
		if in.Overwrite != nil {
			// Explicit overwrite:false (explicit true can't collide).
			return nil, fmt.Errorf("skill:save: %w — skill %q already exists and overwrite=false; change `name:` in SKILL.md to save a new skill, or pass overwrite:true to replace it", skillpkg.ErrSkillExists, manifest.Name)
		}
		// overwrite ABSENT → ask the user; never silently clobber (B54).
		decision, derr := h.inquireOverwrite(ctx, manifest.Name)
		if derr != nil {
			return nil, derr
		}
		switch decision {
		case decideOverwrite:
			if pubErr = h.manager.Publish(ctx, manifest, bundle, skillpkg.PublishOptions{Overwrite: true}); pubErr != nil {
				return nil, fmt.Errorf("skill:save: %w", pubErr)
			}
		case decideRename:
			return nil, fmt.Errorf("skill:save: %w — the user chose to keep both: change `name:` in SKILL.md to a DISTINCT name and call skill:save again (the existing %q was NOT modified)", skillpkg.ErrSkillExists, manifest.Name)
		default: // decideCancel
			return nil, fmt.Errorf("skill:save: %w (the existing %q was NOT modified) — do not retry without a new name or explicit user instruction", errSaveCancelled, manifest.Name)
		}
	} else if pubErr != nil {
		return nil, fmt.Errorf("skill:save: %w", pubErr)
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

	// Reuse the bundle-body file list readBundleBody already produced;
	// the only delta on disk is SKILL.md (always written by Publish), so
	// prepend it rather than re-walking the published bundle a second
	// time.
	files = append(files, "SKILL.md")
	sort.Strings(files)

	return json.Marshal(saveResult{
		Name:      manifest.Name,
		Directory: loaded.Root,
		Files:     files,
		Valid:     true,
	})
}

// errSaveCancelled marks a skill:save the user explicitly cancelled at
// the collision modal — errors.Is-checkable so the caller distinguishes
// "user said no" from a genuine write failure.
var errSaveCancelled = errors.New("skill_save_cancelled")

// isAuthoringValidationError reports whether err is a bundle-CONTENT
// validation failure (the verdict skill:validate produces) as opposed
// to a structural caller error (bad args, missing dir, path escape).
// skill:save uses it to redirect content failures back through
// skill:validate. ErrManifestInvalid wraps every Parse-time failure,
// so it also covers malformed frontmatter / YAML.
func isAuthoringValidationError(err error) bool {
	return errors.Is(err, skillpkg.ErrManifestInvalid) ||
		errors.Is(err, skillpkg.ErrAutoloadReserved) ||
		errors.Is(err, skillpkg.ErrTaskBlockMisplaced) ||
		errors.Is(err, ErrUnknownToolName)
}

// overwriteDecision is the user's pick at the name-collision modal.
type overwriteDecision int

const (
	decideCancel overwriteDecision = iota // default — never write
	decideOverwrite
	decideRename
)

// inquireOverwrite asks the user how to resolve a skill:save name
// collision (overwrite / rename / cancel) via a runtime inquiry. The
// gate lives at the tool, not in a role's prose, so whoever calls
// skill:save hits the same question at the exact save point — a
// separate confirm-role can be out of order, a tool gate cannot (B54).
// When no session is attached to ask (a fixture, a headless fire), it
// fails safe to cancel rather than clobber.
func (h *SessionSkill) inquireOverwrite(ctx context.Context, name string) (overwriteDecision, error) {
	state, ok := extension.SessionStateFromContext(ctx)
	if !ok || state == nil {
		return decideCancel, fmt.Errorf("skill:save: %w — skill %q already exists and `overwrite` was not specified, and there is no attached session to ask the user; re-run with overwrite:true to replace it or change `name:` to save a new skill", skillpkg.ErrSkillExists, name)
	}
	resp, err := state.RequestInquiry(ctx, protocol.InquiryRequestPayload{
		Type:     protocol.InquiryTypeClarification,
		Question: fmt.Sprintf("A skill named %q already exists. Overwrite it, save the new skill under a different name, or cancel?", name),
		Context:  "skill:save — name collision. Overwriting REPLACES the existing skill bundle in the local store (the old one is lost). Pick `rename` to keep both, or `cancel` to abort.",
		Options:  []string{"overwrite", "rename", "cancel"},
	})
	if err != nil {
		return decideCancel, fmt.Errorf("skill:save: could not ask the user about the %q name collision: %w", name, err)
	}
	return interpretOverwriteDecision(resp), nil
}

// interpretOverwriteDecision maps the clarification answer (the chosen
// option text lands in Response) onto a decision. Anything unrecognised
// — a timeout, an empty reply, a free-text "no" — is treated as cancel
// so the existing skill is never overwritten without a clear yes.
func interpretOverwriteDecision(resp *protocol.InquiryResponse) overwriteDecision {
	if resp == nil || resp.Payload.Timeout {
		return decideCancel
	}
	switch s := strings.ToLower(strings.TrimSpace(resp.Payload.Response)); {
	case strings.HasPrefix(s, "overwrite"), s == "o", s == "y", s == "yes", s == "replace":
		return decideOverwrite
	case strings.HasPrefix(s, "rename"), strings.HasPrefix(s, "new"), s == "r":
		return decideRename
	default:
		return decideCancel
	}
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
