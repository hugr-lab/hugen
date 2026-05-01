package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/session"
	"github.com/hugr-lab/hugen/pkg/skill"
	"github.com/hugr-lab/hugen/pkg/tool"
	"github.com/hugr-lab/query-engine/types"
)

// buildSkillStack constructs the SkillStore + SkillManager from the
// installed bundled skills. system tier reads from
// ${StateDir}/skills/system/, local from ${StateDir}/skills/local/.
// CommunityRoot is left empty for now — operator-pinned community
// roots are a config-time extension that lands later.
func buildSkillStack(core *RuntimeCore) (*skill.SkillManager, skill.SkillStore, error) {
	stateDir := core.Boot.StateDir
	if stateDir == "" {
		return nil, nil, fmt.Errorf("buildSkillStack: empty state dir")
	}
	store := skill.NewSkillStore(skill.Options{
		SystemRoot: filepath.Join(stateDir, "skills/system"),
		LocalRoot:  filepath.Join(stateDir, "skills/local"),
	})
	mgr := skill.NewSkillManager(store, core.Logger)
	return mgr, store, nil
}

// buildPermissionService constructs the perm.Service used by the
// ToolManager and consulted by every tool dispatch.
//
// The selector picks Tier-2-aware RemotePermissions when:
//
//   - the deployment opts in via cfg.Permissions().RemoteEnabled();
//     and
//   - a hugr auth source is registered (TokenStore("hugr") works);
//     and
//   - some types.Querier is available to run
//     function.core.auth.my_permissions against. RemoteQuerier is
//     preferred (the Hugr hub is the source of truth for role
//     rules); the local engine falls back when the deployment
//     bundles its own engine.
//
// Otherwise LocalPermissions stays as the Tier-1-only floor — no
// Hugr round-trip on Resolve, no role rules layered on top. The
// permission stack still consults Tier-3 (tool_policies) inside
// ToolManager regardless of which service flavour is wired here.
func buildPermissionService(core *RuntimeCore) perm.Service {
	view := core.Config.Permissions()
	local := perm.NewLocalPermissions(view, core.Identity)

	if !view.RemoteEnabled() {
		return local
	}
	if core.Auth == nil {
		return local
	}
	if _, ok := core.Auth.TokenStore("hugr"); !ok {
		return local
	}
	var q types.Querier
	if core.RemoteQuerier != nil {
		q = core.RemoteQuerier
	} else if core.LocalQuerier != nil {
		q = core.LocalQuerier
	}
	if q == nil {
		return local
	}
	core.Logger.Info("permissions: remote tier enabled (function.core.auth.my_permissions)")
	return perm.NewRemotePermissions(view, core.Identity, newPermQuerier(q))
}

// buildToolStack wires SkillManager + PermissionService + ToolManager
// + SystemProvider, registers any non-MCP provider builders the
// deployment opts into (hugr-query today), then asks the manager to
// open every per_agent entry from cfg.ToolProviders. Per_session
// providers (bash-mcp today) are wired by session.Resources on
// Session.Open via the spawn.Sources / Lifecycle plumbing.
func buildToolStack(core *RuntimeCore, perms perm.Service, skills *skill.SkillManager) (*tool.ToolManager, error) {
	opts := []tool.ToolManagerOption{}

	// hugr-query is a runtime-managed provider type: it spawns the
	// in-tree binary, mints a per-spawn agent-token bootstrap, and
	// revokes it on Close. The store is only built when a `hugr`
	// auth source is registered — US5 no-Hugr deployments skip
	// this whole block and the `type: hugr-query` entry (if any)
	// is rejected at Init with "unknown type", which is the
	// correct degradation: drop the YAML entry to make it clean.
	tokenStore, err := buildAgentTokenStore(core.Auth)
	if err != nil {
		return nil, fmt.Errorf("buildToolStack: agent-token store: %w", err)
	}
	if tokenStore != nil {
		mountAgentTokenHandler(core.Mux, tokenStore)
		// Hand the store to the rest of the runtime so per-session
		// providers with `auth: hugr` (composed via the spawn.Source
		// registry in buildRuntimeCore) share the same loopback
		// exchange surface as hugr-query.
		core.AgentTokenStore = tokenStore
		hq := &hugrQueryBuilder{
			authStore:    tokenStore,
			loopbackPort: core.Boot.Port,
			stateDir:     core.Boot.StateDir,
			log:          core.Logger,
		}
		opts = append(opts, tool.WithProviderBuilder("hugr-query", hq))
	}

	tm := tool.NewToolManager(perms, skills, core.Config.ToolProviders(),
		authResolverFor(core.Auth), core.Logger, opts...)

	var policies *tool.Policies
	if core.LocalQuerier != nil {
		policies = tool.NewPolicies(core.LocalQuerier)
		tm.SetPolicies(policies)
	}

	sys := tool.NewSystemProvider(tool.SystemDeps{
		AgentID:  core.Agent.ID(),
		Notepad:  newNotepadFunc(core.Store),
		Skills:   skills,
		Policies: policies,
		Perms:    perms,
		Reload:   newRuntimeReloadFunc(core, perms, skills, tm),
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

// authResolverFor wraps an *auth.Service into a tool.AuthResolver
// that maps named auth sources to bearer-injecting RoundTrippers.
// This is the only seam between pkg/tool and pkg/auth — pkg/tool
// stays free of any direct auth dependency.
func authResolverFor(svc *auth.Service) tool.AuthResolver {
	return tool.AuthResolverFunc(func(name string) (http.RoundTripper, error) {
		if svc == nil {
			return nil, fmt.Errorf("auth service not initialised")
		}
		ts, ok := svc.TokenStore(name)
		if !ok {
			return nil, fmt.Errorf("auth source %q not registered", name)
		}
		return auth.Transport(ts, http.DefaultTransport), nil
	})
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
// (system + bash + hugr-main + hugr-query) and re-Init's the
// ToolManager so freshly-edited cfg.ToolProviders takes effect.
// The system provider is re-registered alongside.
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

// newNotepadFunc adapts session.Notepad to tool.NotepadFunc.
// AgentID and SessionID are forwarded verbatim from the
// IdentityFromContext-supplied values; the Notepad itself is
// constructed per-call against the shared RuntimeStore.
func newNotepadFunc(store session.RuntimeStore) tool.NotepadFunc {
	return func(ctx context.Context, agentID, sessionID, authorID, text string) (string, error) {
		np := session.NewNotepad(store, agentID, sessionID)
		return np.Append(ctx, authorID, text)
	}
}
