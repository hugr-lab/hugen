package session

import (
	"context"
	"fmt"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/model"
)

// applyChildIntent resolves the freshly-spawned child's default
// intent in two passes:
//
//  1. Tier default — deps.TierIntents[tier] from child.Tier()
//     (Phase 6.1d: caller-supplied SpawnSpec.Tier override
//     survives, else depth-derived default).
//     Phase 4.2.2 §11.
//  2. Per-role override — subagentSpawnHint's Intent (from the
//     dispatching skill's manifest). Wins over the tier default
//     when present.
//
// Unknown intents (not registered with the model router) are
// logged and skipped — child stays on the parent's default. This
// matches the pre-γ behaviour for the role-override branch and
// extends it consistently to the new tier-default branch.
func (parent *Session) applyChildIntent(ctx context.Context, child *Session, skill, role string) {
	if parent.deps == nil || parent.models == nil {
		return
	}
	tier := child.tier

	// 1. Tier default.
	if intentStr, ok := parent.deps.TierIntents[tier]; ok && intentStr != "" {
		intent := model.Intent(intentStr)
		if _, ok := parent.models.SpecFor(intent); ok {
			child.SetDefaultIntent(intent)
		} else {
			parent.logger.Warn("session: tier-default intent unknown to model router; child stays on parent default",
				"parent", parent.id, "child", child.ID(),
				"tier", tier, "intent", intentStr)
		}
	}

	// 2. Per-role override (only when a skill is specified).
	if skill == "" {
		return
	}
	hint := subagentSpawnHint(ctx, parent, skill, role)
	if hint.Intent == "" {
		return
	}
	intent := model.Intent(hint.Intent)
	if _, ok := parent.models.SpecFor(intent); ok {
		child.SetDefaultIntent(intent)
	} else {
		parent.logger.Warn("session: skill role intent unknown to model router; child stays on resolved tier intent",
			"parent", parent.id, "child", child.ID(),
			"skill", skill, "role", role, "intent", hint.Intent)
	}
}

// applyChildSpawnAppliers runs every registered
// [extension.SubagentSpawnApplier] against the freshly-spawned
// child, AFTER applyChildIntent and BEFORE the task UserMessage is
// delivered. The canonical applier today is the skill extension's
// per-role autoload — it Load()s every skill in
// `sub_agents[*].autoload_skills` so the worker's first turn sees
// the surface ready, skipping the explicit `skill:load(...)`
// ritual.
//
// Errors per extension are logged and swallowed; one bad applier
// must not block the spawn, and the worker's prose-level boot
// sequence is a sufficient fallback (it can still skill:load on
// demand).
func (parent *Session) applyChildSpawnAppliers(ctx context.Context, child *Session, skill, role string) {
	if parent.deps == nil {
		return
	}
	for _, ext := range parent.deps.Extensions {
		applier, ok := ext.(extension.SubagentSpawnApplier)
		if !ok {
			continue
		}
		if err := applier.ApplyOnSubagentSpawn(ctx, child, skill, role); err != nil {
			parent.logger.Warn("session: SubagentSpawnApplier failed",
				"extension", fmt.Sprintf("%T", ext),
				"parent", parent.id, "child", child.ID(),
				"skill", skill, "role", role, "err", err)
		}
	}
}
