package runtime

import (
	"context"
	"fmt"
	"time"

	"github.com/hugr-lab/hugen/pkg/session"
)

// phaseSessionManager runs phase 9: assembles the per-session
// Lifecycle (session.Resources) and the agent-level
// session.Manager.
//
// Phase 4.1b-pre stage A retired the "Manager-as-ToolProvider"
// pattern. session:* tools now register on a per-session
// SessionToolProvider whose lifecycle Resources.Acquire owns —
// see ResourceDeps.SessionTools below. Manager itself is not a
// tool.ToolProvider anymore.
//
// Per-session children + per_session provider construction
// happens inside Resources.Acquire on Session.Open; Phase 9 only
// builds the wiring.
func phaseSessionManager(_ context.Context, core *Core) error {
	resources := session.NewResources(session.ResourceDeps{
		Providers:  core.Config.ToolProviders(),
		Tools:      core.Tools,
		Skills:     core.Skills,
		SkillStore: core.SkillStore,
		Workspace:  core.Workspace,
		Logger:     core.Logger,
		// SessionTools is the leaf-deps bag the per-session
		// SessionToolProvider closes over. Constructed once here so
		// every spawned session's Provider sees the same store /
		// logger / permission service.
		SessionTools: session.SessionToolHost{
			Store:  core.Store,
			Logger: core.Logger,
			Perms:  core.Permissions,
		},
	})
	if err := resources.Validate(); err != nil {
		return fmt.Errorf("session resources: %w", err)
	}

	mgr := session.NewManager(
		core.Store, core.Agent, core.Models, core.Commands, core.Codec, core.Logger,
		session.WithLifecycle(resources),
		session.WithSessionOptions(
			session.WithTools(core.Tools),
			session.WithSkills(core.Skills),
		),
	)
	core.Manager = mgr

	// Phase 4.1c step 34 retired the central Reconnector and its
	// OnRecover hook — recovery is now lazy via
	// pkg/tool/providers/recovery.Wrap on the next failed
	// Call/List. The mcp_recovered system_marker broadcast is
	// re-introduced in a later phase if observability still wants
	// per-session visibility into provider transitions.

	core.addCleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		mgr.ShutdownAll(shutdownCtx)
	})
	return nil
}
