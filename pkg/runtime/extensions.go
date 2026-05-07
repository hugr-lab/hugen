package runtime

import (
	"context"
	"fmt"

	"github.com/hugr-lab/hugen/pkg/extension"
	mcpext "github.com/hugr-lab/hugen/pkg/extension/mcp"
	notepadext "github.com/hugr-lab/hugen/pkg/extension/notepad"
	skillext "github.com/hugr-lab/hugen/pkg/extension/skill"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// phaseExtensions runs phase 8.5: builds the runtime's session
// extensions (notepad, plan, whiteboard, skill — added per
// migration), registers each ToolProvider-implementing extension on
// Core.Tools so their catalogue surfaces to every session, and
// merges any Commander-contributed slash commands onto Core.Commands.
//
// Capability hooks beyond ToolProvider / Commander (StateInitializer,
// Recovery, Closer, Advertiser, ToolFilter, FrameRouter) are
// dispatched at runtime by Session.NewSession and friends —
// phaseExtensions only owns construction + agent-level registrations.
//
// Today only the notepad extension migrated to this model; the rest
// still live as session: tools registered directly on Session.
// Adding plan/whiteboard/skill follows the same shape: build
// instance with deps + append to Core.Extensions + (if ToolProvider)
// AddProvider on Core.Tools + (if Commander) Register on
// Core.Commands.
func phaseExtensions(_ context.Context, core *Core) error {
	exts := []extension.Extension{
		notepadext.NewExtension(core.Store, core.Agent.ID()),
		skillext.NewExtension(core.Skills, core.Permissions, core.Agent.ID()),
		// mcpext spawns per_session MCP providers from the
		// agent-level config view. Registered after skill so
		// skill's autoload (which can reference per_session-
		// hosted tools) sees the spawned catalogue once skill's
		// FilterTools runs in fetchSnapshot. Order across
		// extensions only matters for state-write dependencies
		// — neither skill autoload nor mcp spawn reads from the
		// other's state, so the choice here is purely
		// aesthetic.
		mcpext.NewExtension(core.Config.ToolProviders(), core.Logger),
	}

	for _, ext := range exts {
		if p, ok := ext.(tool.ToolProvider); ok {
			if err := core.Tools.AddProvider(p); err != nil {
				return fmt.Errorf("register extension %q as tool provider: %w", ext.Name(), err)
			}
		}
		if cmder, ok := ext.(extension.Commander); ok {
			for _, cmd := range cmder.Commands() {
				if err := core.Commands.Register(cmd.Name, session.CommandSpec{
					Description: cmd.Description,
					Handler:     adaptExtensionCommand(cmd.Handler),
				}); err != nil {
					return fmt.Errorf("register extension %q command %q: %w",
						ext.Name(), cmd.Name, err)
				}
			}
		}
	}

	core.Extensions = exts
	return nil
}

// adaptExtensionCommand bridges an [extension.CommandFn] (which sees
// only [extension.SessionState] + [extension.CommandContext]) to the
// session-level [session.CommandHandler] signature the registry
// expects. The wrapping is a single closure per command — no
// per-call allocation beyond the CommandContext literal.
func adaptExtensionCommand(fn extension.CommandFn) session.CommandHandler {
	return func(ctx context.Context, env session.CommandEnv, args []string) ([]protocol.Frame, error) {
		return fn(ctx, env.Session, extension.CommandContext{
			Author:      env.Author,
			AgentAuthor: env.AgentAuthor,
		}, args)
	}
}
