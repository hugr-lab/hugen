package main

import (
	"context"
	"fmt"
	"time"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/runtime"
	"github.com/hugr-lab/hugen/pkg/skill"
	"github.com/hugr-lab/hugen/pkg/tool"
	"github.com/hugr-lab/hugen/pkg/tool/providers"
	"github.com/hugr-lab/hugen/pkg/tool/providers/admin"
	"github.com/hugr-lab/hugen/pkg/tool/providers/policies"
)

// buildToolStack wires the agent-level ToolManager + the per_agent
// providers (system, policies, admin, runtime:reload) and asks the
// manager to open every per_agent entry from cfg.ToolProviders.
// Per_session providers (bash-mcp today) are wired by
// session.Resources on Session.Open. cmd/hugen knows nothing about
// provider names — every stdio MCP carrying `auth: hugr` lands on
// the auth.Service-owned loopback uniformly.
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
		// Tier-3 management surface: policy:save / policy:revoke land
		// on the policies.Policies provider in
		// pkg/tool/providers/policies.
		pol := policies.New(legacyPolicies, perms, core.Logger)
		if err := tm.AddProvider(pol); err != nil {
			return nil, fmt.Errorf("buildToolStack: register policies provider: %w", err)
		}
	}

	// Registry-mutation surface: tool:provider_add /
	// tool:provider_remove. Mirrors pkg/runtime/tools.go.
	if err := tm.AddProvider(admin.New(tm)); err != nil {
		return nil, fmt.Errorf("buildToolStack: register admin provider: %w", err)
	}

	// runtime:reload — replaces the legacy system:runtime_reload.
	reload := runtime.NewReloadProvider(runtime.ReloadDeps{
		Perms:  perms,
		Skills: skills,
		Tools:  tm,
		Logger: core.Logger,
	})
	if err := tm.AddProvider(reload); err != nil {
		return nil, fmt.Errorf("buildToolStack: register reload provider: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := tm.Init(ctx); err != nil {
		return nil, err
	}
	return tm, nil
}
