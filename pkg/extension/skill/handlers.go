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
	permObjectLoad        = "hugen:tool:system"
	permObjectUnload      = "hugen:tool:system"
	permObjectPublish     = "hugen:tool:system"
	permObjectFiles       = "hugen:tool:system"
	permObjectRef         = "hugen:tool:system"
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

	publishSchema = `{
  "type": "object",
  "properties": {
    "name": {"type": "string"},
    "body": {"type": "string", "description": "Full SKILL.md contents (frontmatter + body)."}
  },
  "required": ["name", "body"]
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
			Name:             providerName + ":publish",
			Description:      "Publish a skill manifest+body into the local store.",
			Provider:         providerName,
			PermissionObject: permObjectPublish,
			ArgSchema:        json.RawMessage(publishSchema),
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
	case "publish":
		return h.callPublish(ctx, args)
	case "files":
		return h.callFiles(ctx, args)
	case "ref":
		return h.callRef(ctx, args)
	default:
		return nil, fmt.Errorf("%w: skill:%s", tool.ErrUnknownTool, short)
	}
}

// Subscribe implements [tool.ToolProvider]. The catalogue is static.
func (e *Extension) Subscribe(_ context.Context) (<-chan tool.ProviderEvent, error) {
	return nil, nil
}

// Close implements [tool.ToolProvider]. Per-session state cleanup
// happens via the [extension.Closer] hook (added in stage 3); the
// provider itself holds no resources.
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
	if err := h.manager.Load(ctx, h.sessionID, in.Name); err != nil {
		return nil, err
	}
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
	if err := h.manager.Unload(ctx, h.sessionID, in.Name); err != nil {
		return nil, err
	}
	return json.RawMessage(`{"unloaded":true}`), nil
}

// ---------- skill:publish ----------

// callPublish remains a stub matching the legacy SystemProvider
// behaviour: inline-body wiring is still pending (deferred to T039
// in the original phase-3 spec). Returning ErrSystemUnavailable
// keeps callers' UX unchanged across the move.
func (h *SessionSkill) callPublish(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
	return nil, fmt.Errorf("%w: skill:publish requires inline body wiring (deferred to T039)", tool.ErrSystemUnavailable)
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
	loaded, err := h.manager.LoadedSkill(ctx, h.sessionID, in.Skill)
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
	loaded, err := h.manager.LoadedSkill(ctx, h.sessionID, in.Name)
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
