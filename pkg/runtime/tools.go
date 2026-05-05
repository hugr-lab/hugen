package runtime

import (
	"context"
	"fmt"
	"time"

	"github.com/hugr-lab/hugen/pkg/session"
	"github.com/hugr-lab/hugen/pkg/tool"
	"github.com/hugr-lab/hugen/pkg/tool/providers"
	"github.com/hugr-lab/hugen/pkg/tool/providers/admin"
	"github.com/hugr-lab/hugen/pkg/tool/providers/policies"
)

// phaseTools runs phase 8: build the per-session Workspace, the
// providers.Builder, and the root *tool.ToolManager. Per_agent
// providers from cfg.ToolProviders() load via Manager.Init (the
// builder dispatches each Spec). The Tier-3 policies store +
// admin registry-mutation provider register here so Resolve and
// `tool:provider_*` tools work the moment Core.Tools is non-nil.
//
// Per_session providers (bash-mcp, ...) stay outside this phase —
// each session opens them on Resources.Acquire via NewChild +
// AddProvider/AddBySpec (pkg/session/lifecycle.go).
func phaseTools(ctx context.Context, core *Core) error {
	core.Workspace = session.NewWorkspace(core.Cfg.Workspace.Dir, core.Cfg.Workspace.CleanupOnClose)

	wsRoot, err := core.Workspace.Root()
	if err != nil {
		return fmt.Errorf("workspace root: %w", err)
	}

	builder := providers.NewBuilder(core.Auth, core.Permissions, wsRoot, core.Logger)
	tm := tool.NewToolManager(core.Permissions, core.Config.ToolProviders(), core.Logger,
		tool.WithBuilder(builder))
	core.Tools = tm
	core.addCleanup(func() {
		if err := tm.Close(); err != nil {
			core.Logger.Warn("cleanup: close tool manager", "err", err)
		}
	})

	// Tier-3 policies store + tool provider. Local-only (the legacy
	// store is DuckDB-backed); deployments without a local engine
	// skip the wiring, leaving Tier-3 disabled (Decide returns
	// PolicyAsk, IsConfigured false → policy:save / policy:revoke
	// surface ErrSystemUnavailable).
	if core.LocalQuerier != nil {
		legacy := tool.NewPolicies(core.LocalQuerier)
		tm.SetPolicies(legacy)
		pol := policies.New(legacy, core.Permissions, core.Logger)
		if err := tm.AddProvider(pol); err != nil {
			return fmt.Errorf("register policies provider: %w", err)
		}
		core.Policies = pol
	}

	// AdminProvider exposes tool:provider_add / tool:provider_remove
	// — the LLM-facing surface for dynamic per_agent registry edits.
	adminP := admin.New(tm)
	if err := tm.AddProvider(adminP); err != nil {
		return fmt.Errorf("register admin provider: %w", err)
	}

	// ReloadProvider hosts runtime:reload (target ∈ permissions /
	// skills / mcp / all).
	reloadP := NewReloadProvider(ReloadDeps{
		Perms:  core.Permissions,
		Skills: core.Skills,
		Tools:  tm,
		Logger: core.Logger,
	})
	if err := tm.AddProvider(reloadP); err != nil {
		return fmt.Errorf("register reload provider: %w", err)
	}

	// Init starts the reconnector loop and loads per_agent providers
	// from cfg.ToolProviders() via the builder (LoadConfig).
	initCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := tm.Init(initCtx); err != nil {
		return fmt.Errorf("tool manager init: %w", err)
	}
	return nil
}
