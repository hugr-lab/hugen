package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
)

// systemProviderName is the fully-qualified prefix used in tool
// names exposed by SystemProvider. Permission objects use
// "hugen:tool:system".
const (
	systemProviderName = "system"
	systemPermObject   = "hugen:tool:system"
)

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
// AgentID is the agent the provider belongs to; downstream
// callbacks read it from SystemDeps once per dispatch.
type SystemDeps struct {
	AgentID string

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
