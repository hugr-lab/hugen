package runtime

import (
	"context"
	"time"

	"github.com/hugr-lab/hugen/pkg/session"
	"github.com/hugr-lab/hugen/pkg/session/manager"
)

// phaseSessionManager runs phase 9: assembles the agent-level
// session.Manager. Workspace + per_session MCP spawn moved to
// extensions (workspace, mcp) in stages 5c / 5b — Manager
// construction no longer wires Resources / Lifecycle.
func phaseSessionManager(_ context.Context, core *Core) error {
	opts := []manager.ManagerOption{
		manager.WithExtensions(core.Extensions...),
		manager.WithSessionOptions(
			session.WithPerms(core.Permissions),
		),
	}
	// Wire phase 4.2.2 §11 tier_intents from config.yaml.models.
	// Empty map is a no-op; populated entries route per-tier child
	// spawns at the model layer.
	if models := core.Config.Models(); models != nil {
		if mc := models.ModelsConfig(); len(mc.TierIntents) > 0 {
			opts = append(opts, manager.WithTierIntents(mc.TierIntents))
		}
	}
	mgr := manager.NewManager(
		core.Store, core.Agent, core.Models, core.Commands, core.Codec, core.Tools, core.Logger,
		opts...,
	)
	core.Manager = mgr

	core.addCleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		mgr.Stop(shutdownCtx)
	})
	return nil
}
