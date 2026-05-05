package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/skill"
	"github.com/hugr-lab/hugen/pkg/tool"
	"github.com/hugr-lab/hugen/pkg/tool/providers"
	"github.com/hugr-lab/hugen/pkg/tool/providers/policies"
)

// buildToolStack wires SkillManager + PermissionService + ToolManager
// + SystemProvider, then asks the manager to open every per_agent
// entry from cfg.ToolProviders. Per_session providers (bash-mcp
// today) are wired by session.Resources on Session.Open. cmd/hugen
// knows nothing about provider names — every stdio MCP carrying
// `auth: hugr` lands on the auth.Service-owned loopback uniformly.
func buildToolStack(core *RuntimeCore, perms perm.Service, skills *skill.SkillManager) (*tool.ToolManager, error) {
	wsRoot := ""
	if core.workspaces != nil {
		// Resolve once; cmd surfaces the error so an unset / unwritable
		// HUGEN_WORKSPACE_DIR isn't silently swallowed by per_agent
		// children that would then fall back to whatever the YAML had.
		root, err := core.workspaces.Root()
		if err != nil {
			return nil, fmt.Errorf("buildToolStack: workspace root: %w", err)
		}
		wsRoot = root
	}
	builder := providers.NewBuilder(core.Auth, perms, wsRoot, core.Logger)
	tm := tool.NewToolManager(perms, core.Config.ToolProviders(), core.Logger,
		tool.WithBuilder(builder))

	var legacyPolicies *tool.Policies
	if core.LocalQuerier != nil {
		legacyPolicies = tool.NewPolicies(core.LocalQuerier)
		tm.SetPolicies(legacyPolicies)
		// Tier-3 management surface: policy:save / policy:revoke
		// land on the new policies.Policies provider in the
		// pkg/tool/providers/policies subpackage. SystemProvider no
		// longer hosts these tools.
		pol := policies.New(legacyPolicies, perms, core.Logger)
		if err := tm.AddProvider(pol); err != nil {
			return nil, fmt.Errorf("buildToolStack: register policies provider: %w", err)
		}
	}

	sys := tool.NewSystemProvider(tool.SystemDeps{
		AgentID: core.Agent.ID(),
		Perms:   perms,
		Reload:  newRuntimeReloadFunc(core, perms, skills, tm),
	})
	if err := tm.AddProvider(sys); err != nil {
		return nil, fmt.Errorf("buildToolStack: register system provider: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := tm.Init(ctx); err != nil {
		return nil, err
	}
	return tm, nil
}

// newRuntimeReloadFunc returns the dispatcher wired into
// SystemDeps.Reload. The system tool runtime_reload accepts
// target ∈ {permissions, skills, mcp, all} after permission
// gating; this function is the per-target router.
//
//   - "permissions" → perm.Service.Refresh — bumps the role
//     snapshot, singleflight-coalesced; LocalPermissions makes
//     this a no-op.
//   - "skills" → SkillManager.RefreshAll — re-reads every loaded
//     skill across every session; bumps the skill generation so
//     ToolManager rebuilds the next snapshot.
//   - "mcp" → drain & remove every per_agent MCP provider, then
//     re-Init from the fresh ToolProviders view.
//   - "all" → all of the above, sequentially. Failures are joined
//     so one subsystem reload does not block the others.
func newRuntimeReloadFunc(core *RuntimeCore, perms perm.Service, skills *skill.SkillManager, tm *tool.ToolManager) func(context.Context, string) error {
	return func(ctx context.Context, target string) error {
		switch target {
		case "permissions":
			return perms.Refresh(ctx)
		case "skills":
			if skills == nil {
				return nil
			}
			_, err := skills.RefreshAll(ctx)
			return err
		case "mcp":
			return reloadMCPProviders(ctx, tm, core)
		case "all":
			var errs []error
			if err := perms.Refresh(ctx); err != nil {
				errs = append(errs, fmt.Errorf("permissions: %w", err))
			}
			if skills != nil {
				if _, err := skills.RefreshAll(ctx); err != nil {
					errs = append(errs, fmt.Errorf("skills: %w", err))
				}
			}
			if err := reloadMCPProviders(ctx, tm, core); err != nil {
				errs = append(errs, fmt.Errorf("mcp: %w", err))
			}
			if len(errs) == 0 {
				return nil
			}
			return joinErrs(errs)
		}
		return fmt.Errorf("runtime_reload: unknown target %q", target)
	}
}

// reloadMCPProviders drains every registered per_agent provider
// and re-Init's the ToolManager so freshly-edited cfg.ToolProviders
// takes effect. The system provider is re-registered alongside.
func reloadMCPProviders(ctx context.Context, tm *tool.ToolManager, core *RuntimeCore) error {
	for _, name := range tm.Providers() {
		if err := tm.RemoveProvider(ctx, name); err != nil {
			core.Logger.Warn("runtime_reload: remove provider", "name", name, "err", err)
		}
	}
	return tm.Init(ctx)
}

func joinErrs(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	if len(errs) == 1 {
		return errs[0]
	}
	return fmt.Errorf("runtime_reload: %w", errors.Join(errs...))
}

