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

// SystemDeps wires the system-tools provider to the surrounding
// runtime. The Reload callback may be nil — runtime_reload then
// surfaces ErrSystemUnavailable on dispatch (Tier-1 may also strip
// it from the catalogue).
//
// AgentID is the agent the provider belongs to; downstream callbacks
// read it from SystemDeps once per dispatch.
type SystemDeps struct {
	AgentID string

	// Perms gates runtime_reload through
	// hugen:command:runtime_reload:<target>. nil falls back to
	// no-gate behaviour (allow) — used in tests.
	Perms perm.Service

	// Reload runs the per-target reload pipeline. nil disables
	// runtime_reload (surfaces ErrSystemUnavailable).
	Reload func(ctx context.Context, target string) error
}

// SystemProvider exposes the runtime's built-in tools as a
// ToolProvider so the catalogue and permission machinery treat
// them uniformly with MCP-backed tools. Lifetime is per-agent.
//
// Phase 4.1a steps 20-24 thinned this provider down: skill_*,
// notepad_append, tool_catalog, policy_*, and mcp_* migrated to
// dedicated providers (`session`, `policy`, `tool`). All that
// remains here is `system:runtime_reload`; phase 4.1a step 25
// pulls that one out into a `runtime` provider before SystemProvider
// itself is deleted (step 27).
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
// in by the runtime.
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
			ErrPermissionDenied, target, deniedTier(got))
	}
	return nil
}
