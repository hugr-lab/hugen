package runtime

import (
	"context"
	"fmt"
	"time"

	"github.com/hugr-lab/hugen/pkg/session"
)

// phaseSessionManager runs phase 9: assembles the per-session
// Lifecycle (session.Resources) and the agent-level
// session.Manager. Registers the Manager as a tool.ToolProvider
// (so session:* tools dispatch through ToolManager.Resolve), and
// wires the MCP reconnector's OnRecover hook to broadcast a
// system_marker into every live root session whenever a
// previously-stale per_agent provider crawls back to healthy.
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
	})
	if err := resources.Validate(); err != nil {
		return fmt.Errorf("session resources: %w", err)
	}

	mgr := session.NewManager(
		core.Store, core.Agent, core.Models, core.Commands, core.Codec, core.Logger,
		session.WithLifecycle(resources),
		session.WithPerms(core.Permissions),
		session.WithSessionOptions(
			session.WithTools(core.Tools),
			session.WithSkills(core.Skills),
		),
	)
	core.Manager = mgr

	// Register Manager as the "session" tool.ToolProvider. The
	// dispatch table (sessionTools) holds notepad_append, plan_*,
	// whiteboard_*, spawn_subagent, skill_*, tool_catalog —
	// everything that needs the calling *Session via
	// SessionFromContext.
	if err := core.Tools.AddProvider(mgr); err != nil {
		return fmt.Errorf("register session provider: %w", err)
	}

	// Reconnector OnRecover hook (phase-4 US7): when a per_agent
	// provider crawls back from stale to healthy, broadcast a
	// system_marker{mcp_recovered, provider} into every live root
	// session's inbox so the model on each root sees the recovery
	// in its transcript and can retry tools that previously
	// surfaced as `provider_removed`.
	if rc := core.Tools.Reconnector(); rc != nil {
		logger := core.Logger
		rc.OnRecover(func(name string) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			mgr.BroadcastSystemMarker(ctx, "mcp_recovered",
				map[string]any{"provider": name})
			logger.Info("mcp reconnect: marker broadcast", "provider", name)
		})
	}

	core.addCleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		mgr.ShutdownAll(shutdownCtx)
	})
	return nil
}
