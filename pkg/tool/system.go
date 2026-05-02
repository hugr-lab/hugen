package tool

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
)

// systemProviderName is the fully-qualified prefix used in tool
// names exposed by SystemProvider ("system:notepad_append" and so
// on). Permission objects use "hugen:tool:system".
const (
	systemProviderName  = "system"
	systemPermObject    = "hugen:tool:system"
	systemSkillsPermObj = "hugen:tool:system" // grouped under one Tier-1 floor
)

// NotepadFunc appends text to the caller's notepad and returns
// the new note id. SystemProvider supplies its own agentID (from
// SystemDeps) and pulls sessionID from perm.SessionFromContext.
type NotepadFunc func(ctx context.Context, agentID, sessionID, authorID, text string) (string, error)

// MCPAddSpec carries the args of mcp_add_server. Caller wires it
// through to the configured registrar; SystemProvider only
// validates JSON shape.
type MCPAddSpec struct {
	Name    string            `json:"name"`
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// SystemDeps wires the system-tools provider to the surrounding
// runtime. Every callback may be nil — the corresponding tool then
// surfaces ErrSystemUnavailable on dispatch (Tier-1 may also strip
// it from the catalogue).
//
// AgentID is the agent the provider belongs to; SystemProvider
// passes it to NotepadFunc on every call without re-resolving the
// identity per dispatch.
type SystemDeps struct {
	AgentID string

	Notepad NotepadFunc

	Skills *skill.SkillManager

	// Policies is the Tier-3 store. nil disables policy_save /
	// policy_revoke (they surface ErrSystemUnavailable). Perms
	// gates access via "hugen:policy:persist" so deployments may
	// forbid persistent overrides for sensitive tools.
	Policies *Policies
	// Perms gates policy_save / policy_revoke through
	// hugen:policy:persist:<tool_name>. nil falls back to no-gate
	// behaviour (allow) — used in tests.
	Perms perm.Service

	AddMCP    func(ctx context.Context, spec MCPAddSpec) error
	RemoveMCP func(ctx context.Context, name string) error
	ReloadMCP func(ctx context.Context, name string) error

	Reload func(ctx context.Context, target string) error
}

// SystemProvider exposes the runtime's built-in tools as a
// ToolProvider so the catalogue and permission machinery treat
// them uniformly with MCP-backed tools. Lifetime is per-agent.
type SystemProvider struct {
	deps SystemDeps
}

// NewSystemProvider constructs the provider. nil-safe for any
// dep — callers wire only the surfaces they actually expose.
func NewSystemProvider(deps SystemDeps) *SystemProvider {
	return &SystemProvider{deps: deps}
}

// ErrSystemUnavailable is returned by SystemProvider.Call when the
// requested system tool's underlying capability was not wired
// in by the runtime (e.g. a no-Hugr deployment registers the
// provider without ReloadMCP).
var ErrSystemUnavailable = errors.New("tool: system tool unavailable in this runtime")

func (p *SystemProvider) Name() string       { return systemProviderName }
func (p *SystemProvider) Lifetime() Lifetime { return LifetimePerAgent }

func (p *SystemProvider) List(ctx context.Context) ([]Tool, error) {
	tools := []Tool{
		{
			Name:             "system:notepad_append",
			Description:      "Append a note to the caller's session notepad.",
			Provider:         systemProviderName,
			PermissionObject: systemPermObject,
			ArgSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "text": {"type": "string", "description": "Note body."},
    "author_id": {"type": "string", "description": "Optional author tag; defaults to the calling identity."}
  },
  "required": ["text"]
}`),
		},
		{
			Name:             "system:skill_load",
			Description:      "Load a skill (and transitive requires) into the caller's session. Use the catalogue from your system prompt to discover available skills.",
			Provider:         systemProviderName,
			PermissionObject: systemPermObject,
			ArgSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {"type": "string", "description": "Skill name as listed in the catalogue (e.g. \"hugr-data\")."}
  },
  "required": ["name"]
}`),
		},
		{
			Name:             "system:skill_unload",
			Description:      "Unload a skill from the caller's session.",
			Provider:         systemProviderName,
			PermissionObject: systemPermObject,
			ArgSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {"type": "string", "description": "Skill name to unload."}
  },
  "required": ["name"]
}`),
		},
		{
			Name:             "system:skill_publish",
			Description:      "Publish a skill manifest+body into the local store.",
			Provider:         systemProviderName,
			PermissionObject: systemPermObject,
			ArgSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {"type": "string"},
    "body": {"type": "string", "description": "Full SKILL.md contents (frontmatter + body)."}
  },
  "required": ["name", "body"]
}`),
		},
		{
			Name:             "system:skill_files",
			Description:      "List on-disk files of a loaded skill with relative + absolute paths so other tools (bash.read_file, python.run_script) can read them. Optional subdir narrows the listing; optional glob filters by path pattern.",
			Provider:         systemProviderName,
			PermissionObject: systemPermObject,
			ArgSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "name":   {"type": "string", "description": "Loaded skill name."},
    "subdir": {"type": "string", "description": "Optional sub-directory under the skill root (e.g. \"references\")."},
    "glob":   {"type": "string", "description": "Optional filepath.Match-flavour glob filter on the relative path (e.g. \"*.md\")."}
  },
  "required": ["name"]
}`),
		},
		{
			Name:             "system:skill_ref",
			Description:      "Read a reference document (references/<ref>.md) from a loaded skill. References are listed in the skill's SKILL.md body.",
			Provider:         systemProviderName,
			PermissionObject: systemPermObject,
			ArgSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "skill": {"type": "string", "description": "Loaded skill name."},
    "ref":   {"type": "string", "description": "Reference base name without the .md extension (e.g. \"instructions\")."}
  },
  "required": ["skill", "ref"]
}`),
		},
		{
			Name:             "system:runtime_reload",
			Description:      "Reload runtime subsystems. target ∈ {permissions, skills, mcp, all}; defaults to all.",
			Provider:         systemProviderName,
			PermissionObject: systemPermObject,
			ArgSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "target": {"type": "string", "enum": ["permissions", "skills", "mcp", "all"]}
  }
}`),
		},
		{
			Name:             "system:mcp_add_server",
			Description:      "Spawn and register an MCP server (admin path).",
			Provider:         systemProviderName,
			PermissionObject: systemPermObject,
			ArgSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {"type": "string"},
    "command": {"type": "string"},
    "args": {"type": "array", "items": {"type": "string"}},
    "env": {"type": "object", "description": "Environment variables as a flat string→string map."}
  },
  "required": ["name", "command"]
}`),
		},
		{
			Name:             "system:mcp_remove_server",
			Description:      "Drain and remove an MCP server.",
			Provider:         systemProviderName,
			PermissionObject: systemPermObject,
			ArgSchema: json.RawMessage(`{
  "type": "object",
  "properties": {"name": {"type": "string"}},
  "required": ["name"]
}`),
		},
		{
			Name:             "system:mcp_reload_server",
			Description:      "Restart a registered MCP server.",
			Provider:         systemProviderName,
			PermissionObject: systemPermObject,
			ArgSchema: json.RawMessage(`{
  "type": "object",
  "properties": {"name": {"type": "string"}},
  "required": ["name"]
}`),
		},
		{
			Name:             "system:policy_save",
			Description:      "Persist a Tier-3 personal policy for a tool. Use this when the user says \"always allow\" / \"always deny\" a tool. The policy never overrides operator floor (Tier 1) or role rules (Tier 2). Returns the composite id required by policy_revoke.",
			Provider:         systemProviderName,
			PermissionObject: systemPermObject,
			ArgSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "tool_name": {"type": "string", "description": "Fully-qualified tool name <provider>:<field>; trailing * accepted as a glob (e.g. \"hugr-main:data-*\")."},
    "decision":  {"type": "string", "enum": ["allow", "deny", "ask"], "description": "allow → run without prompting; deny → block; ask → defer to default."},
    "scope":     {"type": "string", "description": "global (default) | skill:<name> | role:<skill>:<role>"},
    "note":      {"type": "string", "description": "Optional free-form annotation persisted with the row."}
  },
  "required": ["tool_name", "decision"]
}`),
		},
		{
			Name:             "system:policy_revoke",
			Description:      "Delete a Tier-3 personal policy by composite id (returned from policy_save). Idempotent.",
			Provider:         systemProviderName,
			PermissionObject: systemPermObject,
			ArgSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "id": {"type": "string", "description": "Composite policy id (agent|tool_name|scope) returned by policy_save."}
  },
  "required": ["id"]
}`),
		},
	}
	for i := range tools {
		if err := ValidateLLMSchema(tools[i].ArgSchema); err != nil {
			return nil, fmt.Errorf("system tool %q: %w", tools[i].Name, err)
		}
	}
	return tools, nil
}

func (p *SystemProvider) Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	switch name {
	case "notepad_append":
		return p.callNotepadAppend(ctx, args)
	case "skill_load":
		return p.callSkillLoad(ctx, args)
	case "skill_unload":
		return p.callSkillUnload(ctx, args)
	case "skill_publish":
		return nil, fmt.Errorf("%w: skill_publish requires inline body wiring (deferred to T039)", ErrSystemUnavailable)
	case "skill_ref":
		return p.callSkillRef(ctx, args)
	case "skill_files":
		return p.callSkillFiles(ctx, args)
	case "runtime_reload":
		return p.callRuntimeReload(ctx, args)
	case "mcp_add_server":
		return p.callMCPAdd(ctx, args)
	case "mcp_remove_server":
		return p.callMCPRemove(ctx, args)
	case "mcp_reload_server":
		return p.callMCPReload(ctx, args)
	case "policy_save":
		return p.callPolicySave(ctx, args)
	case "policy_revoke":
		return p.callPolicyRevoke(ctx, args)
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnknownTool, name)
	}
}

func (p *SystemProvider) Subscribe(ctx context.Context) (<-chan ProviderEvent, error) {
	return nil, nil
}

func (p *SystemProvider) Close() error { return nil }

func (p *SystemProvider) callNotepadAppend(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if p.deps.Notepad == nil {
		return nil, ErrSystemUnavailable
	}
	var in struct {
		Text     string `json:"text"`
		AuthorID string `json:"author_id,omitempty"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("%w: notepad_append: %v", ErrArgValidation, err)
	}
	sc, _ := perm.SessionFromContext(ctx)
	id, err := p.deps.Notepad(ctx, p.deps.AgentID, sc.SessionID, in.AuthorID, in.Text)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]string{"id": id})
}

func (p *SystemProvider) callSkillLoad(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if p.deps.Skills == nil {
		return nil, ErrSystemUnavailable
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("%w: skill_load: %v", ErrArgValidation, err)
	}
	if in.Name == "" {
		return nil, fmt.Errorf("%w: skill_load: name required", ErrArgValidation)
	}
	sc, _ := perm.SessionFromContext(ctx)
	if sc.SessionID == "" {
		return nil, fmt.Errorf("%w: skill_load: missing session id on context", ErrArgValidation)
	}
	if err := p.deps.Skills.Load(ctx, sc.SessionID, in.Name); err != nil {
		return nil, err
	}
	return json.RawMessage(`{"loaded":true}`), nil
}

func (p *SystemProvider) callSkillUnload(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if p.deps.Skills == nil {
		return nil, ErrSystemUnavailable
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("%w: skill_unload: %v", ErrArgValidation, err)
	}
	sc, _ := perm.SessionFromContext(ctx)
	if err := p.deps.Skills.Unload(ctx, sc.SessionID, in.Name); err != nil {
		return nil, err
	}
	return json.RawMessage(`{"unloaded":true}`), nil
}

func (p *SystemProvider) callSkillRef(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if p.deps.Skills == nil {
		return nil, ErrSystemUnavailable
	}
	var in struct {
		Skill string `json:"skill"`
		Ref   string `json:"ref"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("%w: skill_ref: %v", ErrArgValidation, err)
	}
	if in.Skill == "" || in.Ref == "" {
		return nil, fmt.Errorf("%w: skill_ref: skill and ref required", ErrArgValidation)
	}
	sc, _ := perm.SessionFromContext(ctx)
	s, err := p.deps.Skills.LoadedSkill(ctx, sc.SessionID, in.Skill)
	if err != nil {
		return nil, fmt.Errorf("skill_ref: %w", err)
	}
	if s.FS == nil {
		return nil, fmt.Errorf("skill_ref: %s has no body fs", in.Skill)
	}
	// Look up the ref body. The model addresses references by base
	// name (e.g. "instructions"); the file on disk has a .md
	// extension. Try the as-supplied path first so callers that
	// already passed an explicit extension (or sub-directory) keep
	// working.
	refPath := "references/" + in.Ref
	body, err := readFile(s.FS, refPath)
	if err != nil {
		altPath := refPath + ".md"
		if alt, altErr := readFile(s.FS, altPath); altErr == nil {
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

func (p *SystemProvider) callSkillFiles(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if p.deps.Skills == nil {
		return nil, ErrSystemUnavailable
	}
	var in struct {
		Name   string `json:"name"`
		Subdir string `json:"subdir,omitempty"`
		Glob   string `json:"glob,omitempty"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("%w: skill_files: %v", ErrArgValidation, err)
	}
	if in.Name == "" {
		return nil, fmt.Errorf("%w: skill_files: name required", ErrArgValidation)
	}
	sc, _ := perm.SessionFromContext(ctx)
	if sc.SessionID == "" {
		return nil, fmt.Errorf("%w: skill_files: missing session id", ErrArgValidation)
	}
	if in.Glob != "" {
		// fail-fast on malformed pattern (filepath.Match returns
		// ErrBadPattern only when the pattern itself is invalid).
		if _, err := filepath.Match(in.Glob, ""); err != nil {
			return nil, fmt.Errorf("%w: skill_files: bad glob %q: %v", ErrArgValidation, in.Glob, err)
		}
	}
	if err := p.gateSkillFiles(ctx, in.Name); err != nil {
		return nil, err
	}
	s, err := p.deps.Skills.LoadedSkill(ctx, sc.SessionID, in.Name)
	if err != nil {
		if errors.Is(err, skill.ErrSkillNotFound) {
			return nil, fmt.Errorf("%w: skill not loaded: %s", ErrNotFound, in.Name)
		}
		return nil, fmt.Errorf("skill_files: %w", err)
	}
	out := skillFilesResult{Skill: in.Name, Files: []skillFileEntry{}}
	if s.Root == "" {
		// Inline / hub skill — no on-disk content to surface.
		return json.Marshal(out)
	}
	out.Root = s.Root
	walkStart := s.Root
	if in.Subdir != "" {
		if filepath.IsAbs(in.Subdir) {
			return nil, fmt.Errorf("%w: skill_files: subdir must be relative", ErrPathEscape)
		}
		joined := filepath.Join(s.Root, in.Subdir)
		rel, err := filepath.Rel(s.Root, joined)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("%w: skill_files: subdir escapes skill root", ErrPathEscape)
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
		rel, relErr := filepath.Rel(s.Root, path)
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
		return nil, fmt.Errorf("%w: skill_files: walk %s: %v", ErrIO, walkStart, walkErr)
	}
	sort.Slice(out.Files, func(i, j int) bool { return out.Files[i].Rel < out.Files[j].Rel })
	return json.Marshal(out)
}

// gateSkillFiles consults Tier-1 / Tier-2 for
// hugen:command:skill_files:<skill_name>. Default decision is allow
// (the listing is informational); operators may pin a deny rule for
// sensitive skills.
func (p *SystemProvider) gateSkillFiles(ctx context.Context, name string) error {
	if p.deps.Perms == nil {
		return nil
	}
	got, err := p.deps.Perms.Resolve(ctx, "hugen:command:skill_files", name)
	if err != nil {
		return err
	}
	if got.Disabled {
		return fmt.Errorf("%w: skill_files(%s) denied by %s tier",
			ErrPermissionDenied, name, deniedPolicyTier(got))
	}
	return nil
}

func (p *SystemProvider) callRuntimeReload(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if p.deps.Reload == nil {
		return nil, ErrSystemUnavailable
	}
	var in struct {
		Target string `json:"target"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("%w: runtime_reload: %v", ErrArgValidation, err)
	}
	if in.Target == "" {
		in.Target = "all"
	}
	switch in.Target {
	case "permissions", "skills", "mcp", "all":
	default:
		return nil, fmt.Errorf("%w: runtime_reload target %q (want permissions|skills|mcp|all)",
			ErrArgValidation, in.Target)
	}
	if err := p.gateRuntimeReload(ctx, in.Target); err != nil {
		return nil, err
	}
	if err := p.deps.Reload(ctx, in.Target); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]string{"reloaded": in.Target})
}

// gateRuntimeReload consults Tier-1 / Tier-2 for
// hugen:command:runtime_reload with field=<target>. Operators can
// disable runtime reload entirely (field "*") or scope it to
// specific subsystems (e.g. allow "skills" but not "mcp").
func (p *SystemProvider) gateRuntimeReload(ctx context.Context, target string) error {
	if p.deps.Perms == nil {
		return nil
	}
	got, err := p.deps.Perms.Resolve(ctx, "hugen:command:runtime_reload", target)
	if err != nil {
		return err
	}
	if got.Disabled {
		return fmt.Errorf("%w: runtime_reload(%s) denied by %s tier",
			ErrPermissionDenied, target, deniedPolicyTier(got))
	}
	return nil
}

func (p *SystemProvider) callMCPAdd(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if p.deps.AddMCP == nil {
		return nil, ErrSystemUnavailable
	}
	var spec MCPAddSpec
	if err := json.Unmarshal(args, &spec); err != nil {
		return nil, fmt.Errorf("%w: mcp_add_server: %v", ErrArgValidation, err)
	}
	if spec.Name == "" || spec.Command == "" {
		return nil, fmt.Errorf("%w: mcp_add_server: name and command required", ErrArgValidation)
	}
	if err := p.deps.AddMCP(ctx, spec); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]string{"added": spec.Name})
}

func (p *SystemProvider) callMCPRemove(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if p.deps.RemoveMCP == nil {
		return nil, ErrSystemUnavailable
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("%w: mcp_remove_server: %v", ErrArgValidation, err)
	}
	if in.Name == "" {
		return nil, fmt.Errorf("%w: mcp_remove_server: name required", ErrArgValidation)
	}
	if err := p.deps.RemoveMCP(ctx, in.Name); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]string{"removed": in.Name})
}

func readFile(fsys fs.FS, name string) ([]byte, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func (p *SystemProvider) callPolicySave(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if !p.deps.Policies.IsConfigured() {
		return nil, ErrSystemUnavailable
	}
	var in struct {
		ToolName string `json:"tool_name"`
		Decision string `json:"decision"`
		Scope    string `json:"scope,omitempty"`
		Note     string `json:"note,omitempty"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("%w: policy_save: %v", ErrArgValidation, err)
	}
	if in.ToolName == "" {
		return nil, fmt.Errorf("%w: policy_save: tool_name required", ErrArgValidation)
	}
	out, err := decodeDecision(in.Decision)
	if err != nil {
		return nil, fmt.Errorf("%w: policy_save: %v", ErrArgValidation, err)
	}
	if err := p.gatePolicyPersist(ctx, in.ToolName); err != nil {
		return nil, err
	}
	id, err := p.deps.Policies.Save(ctx, PolicyInput{
		AgentID:   p.deps.AgentID,
		ToolName:  in.ToolName,
		Scope:     in.Scope,
		Decision:  out,
		Note:      in.Note,
		CreatedBy: PolicyCreatorLLM,
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]string{
		"id":        id,
		"tool_name": in.ToolName,
		"decision":  out.String(),
	})
}

func (p *SystemProvider) callPolicyRevoke(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if !p.deps.Policies.IsConfigured() {
		return nil, ErrSystemUnavailable
	}
	var in struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("%w: policy_revoke: %v", ErrArgValidation, err)
	}
	if in.ID == "" {
		return nil, fmt.Errorf("%w: policy_revoke: id required", ErrArgValidation)
	}
	// Mirror the policy_save gate: parse tool_name out of the
	// composite id so Tier-1/2 can forbid revoke of policies on
	// sensitive tools (otherwise an LLM with revoke access could
	// just delete a deny operator pinned via policy_save). The id
	// shape is `agentID|toolName|scope` (see policyID).
	_, toolName, _, perr := parsePolicyID(in.ID)
	if perr != nil {
		return nil, fmt.Errorf("%w: policy_revoke: %v", ErrArgValidation, perr)
	}
	if err := p.gatePolicyPersist(ctx, toolName); err != nil {
		return nil, err
	}
	if err := p.deps.Policies.Revoke(ctx, in.ID); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]string{"revoked": in.ID})
}

// gatePolicyPersist consults Tier-1 / Tier-2 for the
// hugen:policy:persist permission with field=<tool_name>.
// Deployments may forbid persistent overrides on sensitive tools
// from config; this is the seam for that. nil Perms == allow.
func (p *SystemProvider) gatePolicyPersist(ctx context.Context, toolName string) error {
	if p.deps.Perms == nil {
		return nil
	}
	got, err := p.deps.Perms.Resolve(ctx, "hugen:policy:persist", toolName)
	if err != nil {
		return err
	}
	if got.Disabled {
		return fmt.Errorf("%w: policy_save denied for %q by %s tier",
			ErrPermissionDenied, toolName, deniedPolicyTier(got))
	}
	return nil
}

func deniedPolicyTier(p perm.Permission) string {
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

func decodeDecision(s string) (PolicyOutcome, error) {
	switch s {
	case "allow", "always_allowed":
		return PolicyAllow, nil
	case "deny", "denied":
		return PolicyDeny, nil
	case "ask", "manual_required", "":
		return PolicyAsk, nil
	}
	return PolicyAsk, fmt.Errorf("unknown decision %q", s)
}

func (p *SystemProvider) callMCPReload(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if p.deps.ReloadMCP == nil {
		return nil, ErrSystemUnavailable
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("%w: mcp_reload_server: %v", ErrArgValidation, err)
	}
	if in.Name == "" {
		return nil, fmt.Errorf("%w: mcp_reload_server: name required", ErrArgValidation)
	}
	if err := p.deps.ReloadMCP(ctx, in.Name); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]string{"reloaded": in.Name})
}
