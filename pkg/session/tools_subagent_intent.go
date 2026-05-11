package session

import (
	"context"

	"github.com/hugr-lab/hugen/pkg/model"
	skillpkg "github.com/hugr-lab/hugen/pkg/skill"
)

// applyChildIntent resolves the freshly-spawned child's default
// intent in two passes:
//
//  1. Tier default — deps.TierIntents[tier] from the child's depth.
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
	tier := skillpkg.TierFromDepth(child.depth)

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
