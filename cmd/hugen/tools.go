package main

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"

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
// + SystemProvider. Returns the manager so adapters and the Session
// Turn loop can resolve and dispatch tool calls.
func buildToolStack(core *RuntimeCore, perms perm.Service, skills *skill.SkillManager) (*tool.ToolManager, error) {
	tm := tool.NewToolManager(perms, skills, tool.Options{Logger: core.Logger})

	sys := tool.NewSystemProvider(tool.SystemDeps{
		AgentID: core.Agent.ID(),
		Notepad: newNotepadFunc(core.Store),
		Skills:  skills,
		// AddMCP/RemoveMCP/ReloadMCP/Reload are wired by the
		// session-scoped bash-mcp lifecycle in T040 part 2; until
		// then SystemProvider routes those calls to ErrSystemUnavailable.
	})
	if err := tm.AddProvider(sys); err != nil {
		return nil, fmt.Errorf("buildToolStack: register system provider: %w", err)
	}
	return tm, nil
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

// mountAgentTokenStub mounts /api/auth/agent-token on the http
// adapter and answers every request with 501. The real handler
// lands in US2 alongside hugr-query (T044).
func mountAgentTokenStub(mux *http.ServeMux) {
	mux.HandleFunc("/api/auth/agent-token", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "agent-token endpoint not yet wired (US2)", http.StatusNotImplemented)
	})
}
