package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/runtime"
	"github.com/hugr-lab/hugen/pkg/skill"
	"github.com/hugr-lab/hugen/pkg/tool"
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

// buildPermissionService constructs the Tier-1 LocalPermissions
// service from the per-domain Permissions view. Tier-2
// (RemotePermissions) is wired in US4; Tier-3 (tool_policies)
// hangs off ToolManager itself. The identity source supplies the
// agent id used by template substitution; Role stays empty until
// US4 layers a my_permissions snapshot on top.
func buildPermissionService(core *RuntimeCore) perm.Service {
	return perm.NewLocalPermissions(core.Config.Permissions(), core.Identity)
}

// buildToolStack wires SkillManager + PermissionService + ToolManager
// + SystemProvider, registers any non-MCP provider builders the
// deployment opts into (hugr-query today), then asks the manager to
// open every per_agent entry from cfg.ToolProviders. Per_session
// providers (bash-mcp today) are wired by buildSessionLifecycle on
// Session.Open.
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
		hq := &hugrQueryBuilder{
			authStore:    tokenStore,
			loopbackPort: core.Boot.Port,
			stateDir:     core.Boot.StateDir,
			sharedDir:    os.Getenv("HUGEN_SHARED_ROOT"),
			agentID:      core.Agent.ID(),
			log:          core.Logger,
		}
		opts = append(opts, tool.WithProviderBuilder("hugr-query", hq))
	}

	tm := tool.NewToolManager(perms, skills, core.Config.ToolProviders(),
		authResolverFor(core.Auth), core.Logger, opts...)

	sys := tool.NewSystemProvider(tool.SystemDeps{
		AgentID: core.Agent.ID(),
		Notepad: newNotepadFunc(core.Store),
		Skills:  skills,
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

// newNotepadFunc adapts runtime.Notepad to tool.NotepadFunc.
// AgentID and SessionID are forwarded verbatim from the
// IdentityFromContext-supplied values; the Notepad itself is
// constructed per-call against the shared RuntimeStore.
func newNotepadFunc(store runtime.RuntimeStore) tool.NotepadFunc {
	return func(ctx context.Context, agentID, sessionID, authorID, text string) (string, error) {
		np := runtime.NewNotepad(store, agentID, sessionID)
		return np.Append(ctx, authorID, text)
	}
}

