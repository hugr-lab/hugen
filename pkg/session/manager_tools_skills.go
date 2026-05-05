package session

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
	"github.com/hugr-lab/hugen/pkg/skill"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// Step 20 of phase-4.1a-spec.md §9 — the five skill_* tools move off
// SystemProvider onto the Manager. Handlers run in the caller's
// goroutine via the sessionTools dispatch table; the calling
// *Session is recovered from ctx (its skills field is the shared
// SkillManager wired by cmd/hugen via WithSkills). skill_files
// additionally gates on hugen:command:skill_files via m.perms.

func init() {
	sessionTools["skill_load"] = sessionToolDescriptor{
		Name:             "skill_load",
		Description:      "Load a skill (and transitive requires) into the caller's session. Use the catalogue from your system prompt to discover available skills.",
		PermissionObject: permObjectSkillLoad,
		ArgSchema:        json.RawMessage(skillLoadSchema),
		Handler:          callSkillLoad,
	}
	sessionTools["skill_unload"] = sessionToolDescriptor{
		Name:             "skill_unload",
		Description:      "Unload a skill from the caller's session.",
		PermissionObject: permObjectSkillUnload,
		ArgSchema:        json.RawMessage(skillUnloadSchema),
		Handler:          callSkillUnload,
	}
	sessionTools["skill_publish"] = sessionToolDescriptor{
		Name:             "skill_publish",
		Description:      "Publish a skill manifest+body into the local store.",
		PermissionObject: permObjectSkillPublish,
		ArgSchema:        json.RawMessage(skillPublishSchema),
		Handler:          callSkillPublish,
	}
	sessionTools["skill_files"] = sessionToolDescriptor{
		Name:             "skill_files",
		Description:      "List on-disk files of a loaded skill with relative + absolute paths so other tools (bash.read_file, python.run_script) can read them. Optional subdir narrows the listing; optional glob filters by path pattern.",
		PermissionObject: permObjectSkillFiles,
		ArgSchema:        json.RawMessage(skillFilesSchema),
		Handler:          callSkillFiles,
	}
	sessionTools["skill_ref"] = sessionToolDescriptor{
		Name:             "skill_ref",
		Description:      "Read a reference document (references/<ref>.md) from a loaded skill. References are listed in the skill's SKILL.md body.",
		PermissionObject: permObjectSkillRef,
		ArgSchema:        json.RawMessage(skillRefSchema),
		Handler:          callSkillRef,
	}
}

// Permission objects keep the same hugen:tool:system grouping the
// SystemProvider used so existing operator floors (`hugen:tool:system`
// disabled across the board) keep applying without rewrites.
const (
	permObjectSkillLoad    = "hugen:tool:system"
	permObjectSkillUnload  = "hugen:tool:system"
	permObjectSkillPublish = "hugen:tool:system"
	permObjectSkillFiles   = "hugen:tool:system"
	permObjectSkillRef     = "hugen:tool:system"
)

const (
	skillLoadSchema = `{
  "type": "object",
  "properties": {
    "name": {"type": "string", "description": "Skill name as listed in the catalogue (e.g. \"hugr-data\")."}
  },
  "required": ["name"]
}`

	skillUnloadSchema = `{
  "type": "object",
  "properties": {
    "name": {"type": "string", "description": "Skill name to unload."}
  },
  "required": ["name"]
}`

	skillPublishSchema = `{
  "type": "object",
  "properties": {
    "name": {"type": "string"},
    "body": {"type": "string", "description": "Full SKILL.md contents (frontmatter + body)."}
  },
  "required": ["name", "body"]
}`

	skillFilesSchema = `{
  "type": "object",
  "properties": {
    "name":   {"type": "string", "description": "Loaded skill name."},
    "subdir": {"type": "string", "description": "Optional sub-directory under the skill root (e.g. \"references\")."},
    "glob":   {"type": "string", "description": "Optional filepath.Match-flavour glob filter on the relative path (e.g. \"*.md\")."}
  },
  "required": ["name"]
}`

	skillRefSchema = `{
  "type": "object",
  "properties": {
    "skill": {"type": "string", "description": "Loaded skill name."},
    "ref":   {"type": "string", "description": "Reference base name without the .md extension (e.g. \"instructions\")."}
  },
  "required": ["skill", "ref"]
}`
)

// skillFilesMaxEntries caps the listing per the contract (SC-010).
// Beyond this, the result envelope sets truncated=true so the model
// narrows with subdir / glob and re-calls.
const skillFilesMaxEntries = 1000

type skillFileEntry struct {
	Rel  string `json:"rel"`
	Abs  string `json:"abs"`
	Size int64  `json:"size"`
	Mode string `json:"mode"`
}

type skillFilesResult struct {
	Skill     string           `json:"skill"`
	Root      string           `json:"root"`
	Files     []skillFileEntry `json:"files"`
	Truncated bool             `json:"truncated,omitempty"`
}

// ---------- skill_load ----------

func callSkillLoad(ctx context.Context, _ *Manager, args json.RawMessage) (json.RawMessage, error) {
	s, errFrame, err := callerSession(ctx)
	if errFrame != nil || err != nil {
		return errFrame, err
	}
	if s.skills == nil {
		return nil, tool.ErrSystemUnavailable
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("%w: skill_load: %v", tool.ErrArgValidation, err)
	}
	if in.Name == "" {
		return nil, fmt.Errorf("%w: skill_load: name required", tool.ErrArgValidation)
	}
	if err := s.skills.Load(ctx, s.id, in.Name); err != nil {
		return nil, err
	}
	return json.RawMessage(`{"loaded":true}`), nil
}

// ---------- skill_unload ----------

func callSkillUnload(ctx context.Context, _ *Manager, args json.RawMessage) (json.RawMessage, error) {
	s, errFrame, err := callerSession(ctx)
	if errFrame != nil || err != nil {
		return errFrame, err
	}
	if s.skills == nil {
		return nil, tool.ErrSystemUnavailable
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("%w: skill_unload: %v", tool.ErrArgValidation, err)
	}
	if err := s.skills.Unload(ctx, s.id, in.Name); err != nil {
		return nil, err
	}
	return json.RawMessage(`{"unloaded":true}`), nil
}

// ---------- skill_publish ----------

// skill_publish remains a stub matching SystemProvider's behaviour:
// inline-body wiring is still pending (deferred to T039 in the
// original phase-3 spec). Returning ErrSystemUnavailable keeps
// callers' UX unchanged across the move.
func callSkillPublish(_ context.Context, _ *Manager, _ json.RawMessage) (json.RawMessage, error) {
	return nil, fmt.Errorf("%w: skill_publish requires inline body wiring (deferred to T039)", tool.ErrSystemUnavailable)
}

// ---------- skill_ref ----------

func callSkillRef(ctx context.Context, _ *Manager, args json.RawMessage) (json.RawMessage, error) {
	s, errFrame, err := callerSession(ctx)
	if errFrame != nil || err != nil {
		return errFrame, err
	}
	if s.skills == nil {
		return nil, tool.ErrSystemUnavailable
	}
	var in struct {
		Skill string `json:"skill"`
		Ref   string `json:"ref"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("%w: skill_ref: %v", tool.ErrArgValidation, err)
	}
	if in.Skill == "" || in.Ref == "" {
		return nil, fmt.Errorf("%w: skill_ref: skill and ref required", tool.ErrArgValidation)
	}
	loaded, err := s.skills.LoadedSkill(ctx, s.id, in.Skill)
	if err != nil {
		return nil, fmt.Errorf("skill_ref: %w", err)
	}
	if loaded.FS == nil {
		return nil, fmt.Errorf("skill_ref: %s has no body fs", in.Skill)
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
		return nil, fmt.Errorf("skill_ref: %s/%s: %w", in.Skill, in.Ref, err)
	}
	return json.Marshal(map[string]string{"skill": in.Skill, "ref": in.Ref, "body": string(body)})
}

// ---------- skill_files ----------

func callSkillFiles(ctx context.Context, m *Manager, args json.RawMessage) (json.RawMessage, error) {
	s, errFrame, err := callerSession(ctx)
	if errFrame != nil || err != nil {
		return errFrame, err
	}
	if s.skills == nil {
		return nil, tool.ErrSystemUnavailable
	}
	var in struct {
		Name   string `json:"name"`
		Subdir string `json:"subdir,omitempty"`
		Glob   string `json:"glob,omitempty"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("%w: skill_files: %v", tool.ErrArgValidation, err)
	}
	if in.Name == "" {
		return nil, fmt.Errorf("%w: skill_files: name required", tool.ErrArgValidation)
	}
	if in.Glob != "" {
		// fail-fast on malformed pattern (filepath.Match returns
		// ErrBadPattern only when the pattern itself is invalid).
		if _, err := filepath.Match(in.Glob, ""); err != nil {
			return nil, fmt.Errorf("%w: skill_files: bad glob %q: %v", tool.ErrArgValidation, in.Glob, err)
		}
	}
	if err := gateSkillFiles(ctx, m.perms, in.Name); err != nil {
		return nil, err
	}
	loaded, err := s.skills.LoadedSkill(ctx, s.id, in.Name)
	if err != nil {
		if errors.Is(err, skill.ErrSkillNotFound) {
			return nil, fmt.Errorf("%w: skill not loaded: %s", tool.ErrNotFound, in.Name)
		}
		return nil, fmt.Errorf("skill_files: %w", err)
	}
	out := skillFilesResult{Skill: in.Name, Files: []skillFileEntry{}}
	if loaded.Root == "" {
		// Inline / hub skill — no on-disk content to surface.
		return json.Marshal(out)
	}
	out.Root = loaded.Root
	walkStart := loaded.Root
	if in.Subdir != "" {
		if filepath.IsAbs(in.Subdir) {
			return nil, fmt.Errorf("%w: skill_files: subdir must be relative", tool.ErrPathEscape)
		}
		joined := filepath.Join(loaded.Root, in.Subdir)
		rel, err := filepath.Rel(loaded.Root, joined)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("%w: skill_files: subdir escapes skill root", tool.ErrPathEscape)
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
		out.Files = append(out.Files, skillFileEntry{
			Rel:  rel,
			Abs:  path,
			Size: info.Size(),
			Mode: fmt.Sprintf("0%o", info.Mode().Perm()),
		})
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, fs.SkipAll) {
		return nil, fmt.Errorf("%w: skill_files: walk %s: %v", tool.ErrIO, walkStart, walkErr)
	}
	sort.Slice(out.Files, func(i, j int) bool { return out.Files[i].Rel < out.Files[j].Rel })
	return json.Marshal(out)
}

// gateSkillFiles consults Tier-1 / Tier-2 for
// hugen:command:skill_files:<skill_name>. Default decision is allow
// (the listing is informational); operators may pin a deny rule for
// sensitive skills.
func gateSkillFiles(ctx context.Context, perms perm.Service, name string) error {
	if perms == nil {
		return nil
	}
	got, err := perms.Resolve(ctx, "hugen:command:skill_files", name)
	if err != nil {
		return err
	}
	if got.Disabled {
		return fmt.Errorf("%w: skill_files(%s) denied by %s tier",
			tool.ErrPermissionDenied, name, deniedSkillFilesTier(got))
	}
	return nil
}

func deniedSkillFilesTier(p perm.Permission) string {
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
