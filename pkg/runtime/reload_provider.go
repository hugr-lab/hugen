package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/skill"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// reloadProviderName is the prefix every Tool exposed by
// ReloadProvider carries. Phase 4.1a strict-renames
// system:runtime_reload to runtime:reload; PermissionObject moves
// from hugen:command:runtime_reload to hugen:runtime:reload.
const reloadProviderName = "runtime"

// ReloadDeps is the wiring ReloadProvider needs to fulfil the four
// reload targets. Any nil dep makes the corresponding target a
// no-op (logged at Debug). Perms is consulted for the per-target
// gate; nil leaves the gate fully permissive (used by tests).
type ReloadDeps struct {
	Perms  perm.Service
	Skills *skill.SkillManager
	Tools  *tool.ToolManager
	Logger *slog.Logger
}

// ReloadProvider owns the runtime:reload tool — the LLM-callable
// surface that re-reads live runtime state without restarting the
// process. Dispatch lives natively in pkg/runtime so cmd/hugen has
// no per-target switch to assemble.
type ReloadProvider struct {
	deps ReloadDeps
}

// NewReloadProvider constructs a ReloadProvider. Logger nil falls
// back to slog.Default so callers can pass a zero ReloadDeps in
// tests.
func NewReloadProvider(deps ReloadDeps) *ReloadProvider {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &ReloadProvider{deps: deps}
}

// Name implements tool.ToolProvider. Matches the prefix of every
// Tool the provider exposes ("runtime:reload").
func (p *ReloadProvider) Name() string { return reloadProviderName }

// Lifetime classifies the provider as agent-scoped — one instance
// per agent persists across sessions.
func (p *ReloadProvider) Lifetime() tool.Lifetime { return tool.LifetimePerAgent }

// Subscribe is a no-op — the catalogue is static.
func (p *ReloadProvider) Subscribe(context.Context) (<-chan tool.ProviderEvent, error) {
	return nil, nil
}

// Close releases nothing — every dep is owned by the surrounding
// runtime.
func (p *ReloadProvider) Close() error { return nil }

// List returns the single runtime:reload tool.
func (p *ReloadProvider) List(context.Context) ([]tool.Tool, error) {
	return []tool.Tool{
		{
			Name:             "runtime:reload",
			Description:      "Reload runtime subsystems without restarting the process. target ∈ {permissions, skills, mcp, all}; defaults to all.",
			Provider:         reloadProviderName,
			PermissionObject: "hugen:runtime:reload",
			ArgSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "target": {"type": "string", "enum": ["permissions", "skills", "mcp", "all"]}
  }
}`),
		},
	}, nil
}

// Call dispatches the single tool. Args have already been validated
// by ToolManager.Resolve before reaching here.
func (p *ReloadProvider) Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	if name != "reload" {
		return nil, fmt.Errorf("%w: %s", tool.ErrUnknownTool, name)
	}
	var in struct {
		Target string `json:"target"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, fmt.Errorf("%w: runtime:reload: %v", tool.ErrArgValidation, err)
		}
	}
	if in.Target == "" {
		in.Target = "all"
	}
	switch in.Target {
	case "permissions", "skills", "mcp", "all":
	default:
		return nil, fmt.Errorf("%w: runtime:reload target %q (want permissions|skills|mcp|all)",
			tool.ErrArgValidation, in.Target)
	}
	if err := p.gate(ctx, in.Target); err != nil {
		return nil, err
	}
	if err := p.run(ctx, in.Target); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]string{"reloaded": in.Target})
}

// gate consults Tier-1 / Tier-2 for hugen:runtime:reload with
// field=<target>. Operators can disable the call entirely (field
// "*") or scope it (e.g. allow "skills" but not "mcp").
func (p *ReloadProvider) gate(ctx context.Context, target string) error {
	if p.deps.Perms == nil {
		return nil
	}
	got, err := p.deps.Perms.Resolve(ctx, "hugen:runtime:reload", target)
	if err != nil {
		return err
	}
	if got.Disabled {
		return fmt.Errorf("%w: runtime:reload(%s) denied", tool.ErrPermissionDenied, target)
	}
	return nil
}

// run dispatches the per-target reload pipeline. Failures from
// individual subsystems for target=all are joined so one broken
// dep does not block the others.
func (p *ReloadProvider) run(ctx context.Context, target string) error {
	switch target {
	case "permissions":
		if p.deps.Perms == nil {
			return nil
		}
		return p.deps.Perms.Refresh(ctx)
	case "skills":
		if p.deps.Skills == nil {
			return nil
		}
		_, err := p.deps.Skills.RefreshAll(ctx)
		return err
	case "mcp":
		return p.reloadMCP(ctx)
	case "all":
		var errs []error
		if p.deps.Perms != nil {
			if err := p.deps.Perms.Refresh(ctx); err != nil {
				errs = append(errs, fmt.Errorf("permissions: %w", err))
			}
		}
		if p.deps.Skills != nil {
			if _, err := p.deps.Skills.RefreshAll(ctx); err != nil {
				errs = append(errs, fmt.Errorf("skills: %w", err))
			}
		}
		if err := p.reloadMCP(ctx); err != nil {
			errs = append(errs, fmt.Errorf("mcp: %w", err))
		}
		if len(errs) == 0 {
			return nil
		}
		return errors.Join(errs...)
	}
	return fmt.Errorf("runtime:reload: unreachable target %q", target)
}

// reloadMCP drains every registered per_agent provider and re-runs
// Init so freshly-edited cfg.ToolProviders takes effect. Per-agent
// providers re-spawn through the wired Builder via AddBySpec.
// Per_session providers are owned by sessions; reloads do not touch
// them.
func (p *ReloadProvider) reloadMCP(ctx context.Context) error {
	if p.deps.Tools == nil {
		return nil
	}
	for _, name := range p.deps.Tools.Providers() {
		if err := p.deps.Tools.RemoveProvider(ctx, name); err != nil {
			p.deps.Logger.Warn("runtime:reload: remove provider", "name", name, "err", err)
		}
	}
	return p.deps.Tools.Init(ctx)
}

// ensure ReloadProvider satisfies tool.ToolProvider.
var _ tool.ToolProvider = (*ReloadProvider)(nil)
