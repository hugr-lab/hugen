package runtime

import (
	"context"
	"fmt"

	"github.com/hugr-lab/hugen/pkg/extension"
	notepadext "github.com/hugr-lab/hugen/pkg/extension/notepad"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// phaseExtensions runs phase 8.5: builds the runtime's session
// extensions (notepad, plan, whiteboard, skill — added per
// migration) and registers each ToolProvider-implementing
// extension on Core.Tools so their catalogue surfaces to every
// session through the standard ToolManager parent walk.
//
// Capability hooks beyond ToolProvider (StateInitializer, Recovery,
// Closer, Advertiser, ToolFilter, FrameRouter) are dispatched at
// runtime by Session.NewSession and friends — phaseExtensions only
// owns construction + ToolManager registration.
//
// Today only the notepad extension migrated to this model; the
// rest still live as session: tools registered directly on
// Session. Adding plan/whiteboard/skill follows the same shape:
// build instance with deps + append to Core.Extensions + (if
// ToolProvider) AddProvider on Core.Tools.
func phaseExtensions(_ context.Context, core *Core) error {
	exts := []extension.Extension{
		notepadext.NewExtension(core.Store, core.Agent.ID()),
	}

	for _, ext := range exts {
		if p, ok := ext.(tool.ToolProvider); ok {
			if err := core.Tools.AddProvider(p); err != nil {
				return fmt.Errorf("register extension %q as tool provider: %w", ext.Name(), err)
			}
		}
	}

	core.Extensions = exts
	return nil
}
