package runtime

import (
	"context"
	"fmt"
	"io/fs"
	"time"

	"github.com/hugr-lab/hugen/assets"
	"github.com/hugr-lab/hugen/pkg/config"
	"github.com/hugr-lab/hugen/pkg/prompts"
	"github.com/hugr-lab/hugen/pkg/session"
	"github.com/hugr-lab/hugen/pkg/session/manager"
)

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
	// Wire phase 5.1 § 4.5 async-mission cap from
	// config.yaml.subagents.max_async_missions_per_root. The
	// StaticService default (5) lands here when the YAML omits
	// the field; an explicit 0 means "unlimited".
	if subs := core.Config.Subagents(); subs != nil {
		opts = append(opts, manager.WithMaxAsyncMissionsPerRoot(subs.MaxAsyncMissionsPerRoot()))
		// Phase 5.2 δ (B3): per-tier turn-loop defaults flow from
		// config.yaml.subagents.tier_defaults through SubagentsView.
		// StaticService materialises root / mission / worker with
		// the runtime constants when the YAML block is omitted, so
		// this always installs a populated map.
		if td := subs.TierDefaults(); len(td) > 0 {
			opts = append(opts, manager.WithTierDefaults(projectTierDefaults(td)))
		}
		// Phase 5.2 ε: parking ceiling + idle timeout from
		// config.yaml.subagents.parking. StaticService materialises
		// the defaults (3 / 10m) when the YAML block is omitted.
		opts = append(opts,
			manager.WithMaxParkedChildrenPerRoot(subs.MaxParkedChildrenPerRoot()),
			manager.WithParkedIdleTimeout(subs.ParkedIdleTimeout()),
		)
	}
	// Wire phase 5.1 § 2.7 HITL inquire deadline from
	// config.yaml.hitl.default_timeout_ms.
	if hitl := core.Config.Hitl(); hitl != nil {
		opts = append(opts, manager.WithDefaultInquireTimeoutMs(hitl.DefaultTimeoutMs()))
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

// projectTierDefaults converts the config-layer view of the
// per-tier turn-loop defaults into the session-layer mirror type.
// Keeps the dependency arrow config → session (pkg/session never
// imports pkg/config). Phase 5.2 δ.
func projectTierDefaults(in map[string]config.TierTurnDefaults) map[string]session.TierTurnDefaults {
	out := make(map[string]session.TierTurnDefaults, len(in))
	for tier, v := range in {
		out[tier] = session.TierTurnDefaults{
			MaxToolTurns:     v.MaxToolTurns,
			MaxToolTurnsHard: v.MaxToolTurnsHard,
			StuckPolicy: session.StuckDetectionDefault{
				RepeatedHash:       v.StuckDetection.RepeatedHash,
				TightDensityCount:  v.StuckDetection.TightDensityCount,
				TightDensityWindow: v.StuckDetection.TightDensityWindow,
				Enabled:            v.StuckDetection.Enabled,
			},
		}
	}
	return out
}

// buildPromptRenderer constructs the agent-level prompts.Renderer
// over the embedded assets.PromptsFS. Templates are core agent
// behaviour wired into the binary — no operator override path.
// Phase 5.1 §α.2; embed-only after 2026-05-13 refresh-fix.
func buildPromptRenderer(core *Core) (*prompts.Renderer, error) {
	embedded, err := fs.Sub(assets.PromptsFS, "prompts")
	if err != nil {
		return nil, fmt.Errorf("runtime: scope prompts FS: %w", err)
	}
	return prompts.NewRenderer(embedded, core.Logger), nil
}
