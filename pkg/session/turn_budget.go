package session

import (
	"context"
	"time"

	"github.com/hugr-lab/hugen/pkg/extension"
	skillpkg "github.com/hugr-lab/hugen/pkg/skill"
)

// TierTurnDefaults is the per-tier turn-loop default budget the
// runtime applies when the spawning skill's manifest (per-role /
// per-mission) makes no opinion. Mirrors the
// config.TierTurnDefaults shape field-for-field so the dependency
// arrow stays config → session: pkg/session never imports
// pkg/config. Phase 5.2 δ (B3 migration).
type TierTurnDefaults struct {
	MaxToolTurns       int
	MaxToolTurnsHard   int
	StuckPolicy        StuckDetectionDefault
}

// StuckDetectionDefault is the per-tier stuck-detection block.
// Today only Enabled is honoured by the runtime; the threshold
// fields are reserved for future plumbing. Phase 5.2 δ.
type StuckDetectionDefault struct {
	RepeatedHash       int
	TightDensityCount  int
	TightDensityWindow time.Duration
	Enabled            *bool
}

// IsEnabled mirrors skill.StuckDetectionPolicy.IsEnabled — default
// true; only an explicit Enabled=&false disables.
func (p StuckDetectionDefault) IsEnabled() bool {
	if p.Enabled == nil {
		return true
	}
	return *p.Enabled
}

// resolveTurnBudget walks the phase-5.2 δ resolution chain for
// the calling session's per-Turn budget. Returns the effective
// (softCap, hardCeiling, stuckDisabled) triplet that
// resolveToolIterCap / resolveHardCeiling / stuckDetectionEnabled
// project field-by-field.
//
// Precedence (first non-zero / non-nil per field wins):
//
//  1. TurnBudgetLookup (skill ext) — spawnSkill's per-role then
//     per-mission knobs from the manager catalog.
//  2. Deps.TierDefaults[tier] — operator-supplied per-tier
//     defaults (config.subagents.tier_defaults).
//  3. ToolPolicyAdvisor (legacy) — max-across-loaded-skills
//     composition of the deprecated top-level SkillManifest
//     MaxTurns / MaxTurnsHard / StuckDetection. Slated for
//     removal in phase 5.3.
//  4. Session-level overrides (s.maxToolIters / maxToolItersHard)
//     set via constructor options.
//  5. defaultMaxToolIterations / × 2 — runtime constants.
//
// Phase 5.2 δ.
func (s *Session) resolveTurnBudget(ctx context.Context) (softCap, hardCeiling int, stuckDisabled bool) {
	tier := skillpkg.TierFromDepth(s.depth)

	// Layer 1: per-role / per-mission via TurnBudgetLookup.
	var lookup extension.TurnBudget
	if s.deps != nil {
		for _, ext := range s.deps.Extensions {
			l, ok := ext.(extension.TurnBudgetLookup)
			if !ok {
				continue
			}
			got := l.ResolveTurnBudget(ctx, s, s.spawnSkill, s.spawnRole)
			if got.SoftCap > 0 && lookup.SoftCap == 0 {
				lookup.SoftCap = got.SoftCap
			}
			if got.HardCeiling > 0 && lookup.HardCeiling == 0 {
				lookup.HardCeiling = got.HardCeiling
			}
			if got.StuckDetection != nil && lookup.StuckDetection == nil {
				lookup.StuckDetection = got.StuckDetection
			}
		}
	}

	// Layer 2: Deps.TierDefaults.
	var tierVal TierTurnDefaults
	if s.deps != nil {
		tierVal = s.deps.TierDefaults[tier]
	}

	// Layer 3: legacy ToolPolicyAdvisor (max across advisors).
	legacy := s.gatherToolPolicy(ctx)

	// Field resolution — each independent. SoftCap always resolves
	// to a non-zero value because the runtime constant is the final
	// fallback. HardCeiling can return 0 when every layer abstains,
	// letting resolveHardCeiling apply the "2 × softCap" rule with
	// the caller-supplied softCap argument (which is the resolved
	// cap *for that user turn*, not necessarily today's chain
	// result; tests pass arbitrary softCap values to exercise the
	// fallback). Phase 5.2 δ.
	softCap = firstNonZero(
		lookup.SoftCap,
		tierVal.MaxToolTurns,
		legacy.SoftCap,
		s.maxToolIters,
		defaultMaxToolIterations,
	)
	hardCeiling = firstNonZero(
		lookup.HardCeiling,
		tierVal.MaxToolTurnsHard,
		legacy.HardCeiling,
		s.maxToolItersHard,
	)

	// Stuck detection: walk the chain looking for an explicit
	// opinion. The first layer that supplies a policy wins.
	stuckDisabled = resolveStuckDisabled(lookup.StuckDetection, tierVal.StuckPolicy, legacy.DisableStuckNudges)
	return softCap, hardCeiling, stuckDisabled
}

// firstNonZero returns the first non-zero argument; falls back to
// the final argument when every preceding value is zero. Used to
// thread the budget resolution chain.
func firstNonZero(vals ...int) int {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}
	return 0
}

// resolveStuckDisabled folds the three "is stuck detection off?"
// signals into one boolean. The lookup layer wins if it has any
// policy (IsEnabled controls); tier default wins next; legacy
// advisor is the final fallback. Phase 5.2 δ.
func resolveStuckDisabled(lookup *extension.StuckDetectionPolicy, tier StuckDetectionDefault, legacyDisable bool) bool {
	if lookup != nil {
		return !lookup.IsEnabled()
	}
	if tier.Enabled != nil {
		return !*tier.Enabled
	}
	return legacyDisable
}
