package runtime

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/hugr-lab/hugen/assets"
	"github.com/hugr-lab/hugen/pkg/prompts"
	"github.com/hugr-lab/hugen/pkg/session"
	"github.com/hugr-lab/hugen/pkg/session/manager"
)

// promptsSubdir is the directory under StateDir where operator
// prompt overrides live (per template, relative path matches
// the bundled tree). Missing files fall through to the embedded
// assets.PromptsFS copy. Phase 5.1 §α.2.
const promptsSubdir = "prompts"

// phaseSessionManager runs phase 9: assembles the agent-level
// session.Manager. Workspace + per_session MCP spawn moved to
// extensions (workspace, mcp) in stages 5c / 5b — Manager
// construction no longer wires Resources / Lifecycle.
func phaseSessionManager(_ context.Context, core *Core) error {
	renderer, err := buildPromptRenderer(core)
	if err != nil {
		return err
	}
	opts := []manager.ManagerOption{
		manager.WithExtensions(core.Extensions...),
		manager.WithSessionOptions(
			session.WithPerms(core.Permissions),
		),
		manager.WithPrompts(renderer),
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

// buildPromptRenderer constructs the agent-level prompts.Renderer
// over the embedded assets.PromptsFS plus a per-state-dir override
// directory (<StateDir>/prompts). Operators drop a same-named
// .tmpl under that directory to shadow the bundled copy on a
// per-template basis at render time. Phase 5.1 §α.2.
func buildPromptRenderer(core *Core) (*prompts.Renderer, error) {
	embedded, err := fs.Sub(assets.PromptsFS, "prompts")
	if err != nil {
		return nil, fmt.Errorf("runtime: scope prompts FS: %w", err)
	}
	var overrideDir string
	if core.Cfg.StateDir != "" {
		overrideDir = filepath.Join(core.Cfg.StateDir, promptsSubdir)
	}
	return prompts.NewRenderer(embedded, overrideDir, core.Logger), nil
}
