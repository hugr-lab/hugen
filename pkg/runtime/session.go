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
	mgr := manager.NewManager(
		core.Store, core.Agent, core.Models, core.Commands, core.Codec, core.Tools, core.Logger,
		manager.WithExtensions(core.Extensions...),
		manager.WithSessionOptions(
			session.WithPerms(core.Permissions),
		),
	)
	core.Manager = mgr

	core.addCleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		mgr.Stop(shutdownCtx)
	})
	return nil
}
