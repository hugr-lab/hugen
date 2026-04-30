package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"

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
    "env": {"type": "object", "additionalProperties": {"type": "string"}}
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
	case "runtime_reload":
		return p.callRuntimeReload(ctx, args)
	case "mcp_add_server":
		return p.callMCPAdd(ctx, args)
	case "mcp_remove_server":
		return p.callMCPRemove(ctx, args)
	case "mcp_reload_server":
		return p.callMCPReload(ctx, args)
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
	if err := p.deps.Reload(ctx, in.Target); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]string{"reloaded": in.Target})
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
