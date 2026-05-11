package skill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing/fstest"

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

	// saveSchema deliberately omits `additionalProperties` on
	// references/scripts/assets — Gemini's tool-schema subset
	// rejects it (see pkg/tool/validate.go and the cross-provider
	// conformance test). The inner shape (open-ended string→string
	// map) is described in each field's `description` so the model
	// picks the right call shape from there.
	saveSchema = `{
  "type": "object",
  "properties": {
    "skill_md": {
      "type": "string",
      "description": "Full SKILL.md content (frontmatter + body markdown). Required. Must parse as a valid Manifest. The manifest must NOT set metadata.hugen.autoload — autoload is reserved for system / admin skills."
    },
    "references": {
      "type": "object",
      "description": "Optional. Map: relative path under references/ (string) → markdown file content (string). Example: {\"howto.md\":\"step-by-step notes\",\"deep/dive.md\":\"appendix\"}. Subdirs allowed; absolute paths and parent-dir references rejected."
    },
    "scripts": {
      "type": "object",
      "description": "Optional. Map: relative path under scripts/ (string) → executable artefact content (string). Example: {\"query.py\":\"print('q')\"}. The saved skill body invokes them via ${SKILL_DIR}/scripts/foo.py + bash:run / python:run_script."
    },
    "assets": {
      "type": "object",
      "description": "Optional. Map: relative path under assets/ (string) → text data file content (string). Example: {\"template.html.tmpl\":\"<html/>\"}. Binary assets are NOT supported in v1."
    },
    "overwrite": {
      "type": "boolean",
      "description": "Default false — collision returns ErrSkillExists; ask the user before retrying with overwrite=true. Within the post-save validation iteration loop the agent may set this without asking."
    }
  },
  "required": ["skill_md"]
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
			Name:             providerName + ":save",
			Description:      "Persist a complete skill bundle (SKILL.md + optional references / scripts / assets) to the local skill store. Auto-loads the saved skill in the current session for immediate use. User-initiated only — do NOT propose this. Follow the `_skill_builder` protocol for naming, generalisation, and mandatory post-save validation.",
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
			Name:             toolNameToolsCatalog,
			Description:      toolDescToolsCatalog,
			Provider:         providerName,
			PermissionObject: permObjectToolsCatalog,
			ArgSchema:        json.RawMessage(toolsCatalogSchema),
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
	case "save":
		return h.callSave(ctx, args)
	case "files":
		return h.callFiles(ctx, args)
	case "ref":
		return h.callRef(ctx, args)
	case "tools_catalog":
		return h.callToolsCatalog(ctx, args)
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

// ---------- skill:save ----------

// saveInput is the parsed argument shape; mirrors saveSchema.
type saveInput struct {
	SkillMD    string            `json:"skill_md"`
	References map[string]string `json:"references,omitempty"`
	Scripts    map[string]string `json:"scripts,omitempty"`
	Assets     map[string]string `json:"assets,omitempty"`
	Overwrite  bool              `json:"overwrite,omitempty"`
}

// saveResult is the JSON envelope returned to the LLM after a
// successful save. The model uses Files to drive its mandatory
// post-save validation (run scripts/* against test parameters);
// Directory is the on-disk root the saved-skill body's
// ${SKILL_DIR}/... references resolve against.
type saveResult struct {
	Name      string   `json:"name"`
	Directory string   `json:"directory,omitempty"`
	Files     []string `json:"files"`
}

// callSave persists a skill bundle to the local store and
// auto-loads it in the current session. See
// design/002-runtime-canonical/phase-4.2-spec.md §3.2.
//
// Error mapping (errors.Is-checkable for the LLM consumer via the
// runtime's tool-error envelope):
//   - tool.ErrArgValidation        — malformed args.
//   - skillpkg.ErrManifestInvalid  — skill_md fails Parse.
//   - skillpkg.ErrAutoloadReserved — manifest sets autoload:true.
//   - skillpkg.ErrInvalidPath      — bundle key escapes safety.
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
	if strings.TrimSpace(in.SkillMD) == "" {
		return nil, fmt.Errorf("%w: skill:save: skill_md is required", tool.ErrArgValidation)
	}

	manifest, err := skillpkg.Parse([]byte(in.SkillMD))
	if err != nil {
		return nil, fmt.Errorf("skill:save: manifest does not parse — fix the SKILL.md frontmatter and re-save: %w", err)
	}
	if manifest.Hugen.Autoload {
		return nil, fmt.Errorf("skill:save: %w — drop `metadata.hugen.autoload` from the manifest and re-save (autoload is reserved for system / admin skills compiled into the binary; local skills load on demand)", skillpkg.ErrAutoloadReserved)
	}

	bundle := fstest.MapFS{}
	for _, cat := range []struct {
		name  string
		files map[string]string
	}{
		{"references", in.References},
		{"scripts", in.Scripts},
		{"assets", in.Assets},
	} {
		for k, v := range cat.files {
			cleaned, err := skillpkg.CleanRelPath(k)
			if err != nil {
				return nil, fmt.Errorf("skill:save: bundle key %q under %s/ rejected — use simple relative paths (no leading /, no .., no hidden segments): %w", k, cat.name, err)
			}
			bundle[cat.name+"/"+cleaned] = &fstest.MapFile{Data: []byte(v)}
		}
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

	files := []string{}
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
