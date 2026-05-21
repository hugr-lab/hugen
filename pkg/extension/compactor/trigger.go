package compactor

import (
	"context"

	"github.com/hugr-lab/hugen/pkg/extension"
)

// OnTurnBoundary implements [extension.TurnBoundaryHook]. Runs
// the hybrid trigger predicate against the FrameObserver-
// maintained boundary index and, if it fires, dispatches the
// compaction pipeline.
//
// Errors from the pipeline are non-fatal: a misbehaving fire is
// logged warn-not-fatal and the next boundary retries. Returning
// nil keeps the turn flowing — compaction failure must never
// stall the model loop.
//
// γ: the per-tier / per-skill / per-role resolved Config is
// computed ONCE per fire and passed through to the pipeline so
// the trigger predicate and the compaction body see consistent
// thresholds even if a /compactor command races with a boundary
// hook.
func (e *Extension) OnTurnBoundary(ctx context.Context, state extension.SessionState) error {
	cfg := e.resolveTierConfig(ctx, state)
	if !e.shouldCompact(state, cfg) {
		return nil
	}
	if err := e.compactWithConfig(ctx, state, cfg); err != nil {
		e.logger.Warn("compactor: compaction failed",
			"session", state.SessionID(), "err", err)
	}
	return nil
}

// shouldCompact is the hybrid trigger predicate (spec §4.2).
// Returns true when both:
//
//   - the session has more completed user-turns than the
//     preserved window can hold, AND
//   - at least one limb (turn-count or token-budget) trips, AND
//   - the anti-thrash gate (MinTurnGap completed turns since the
//     last fire) is satisfied.
//
// γ: cfg is the resolved per-tier / per-skill / per-role
// configuration produced by [Extension.resolveTierConfig]. The
// caller computes it once per fire so the predicate and the
// compaction body see the same thresholds.
func (e *Extension) shouldCompact(state extension.SessionState, cfg Config) bool {
	if !cfg.Enabled {
		return false
	}
	// η.2 — only the summarize strategy drives the LLM compactor.
	// "window" prunes purely in OnFrameEmit, "off" leaves history
	// untouched; both short-circuit the LLM trigger here so
	// non-summarising sessions never spin up the model router.
	if effectiveStrategy(cfg.Strategy) != StrategySummarize {
		return false
	}
	// Trigger requires both a model router (to call the LLM)
	// and a store reader (to fetch the compactable range). α-
	// style boot with nil deps is treated as "disabled" so
	// fixtures that never wire a model stay green.
	if e.deps.Router == nil || e.deps.Store == nil {
		return false
	}
	s := FromState(state)
	if s == nil {
		return false
	}
	// Not enough completed user-turns past the preserved
	// window to compact anything yet.
	if s.BoundaryCount() <= cfg.PreservedRecentTurns {
		return false
	}
	// Anti-thrash gate: require MinTurnGap completed turns
	// since the last fire.
	if prior := s.Digest(); prior != nil {
		completedSinceLast := 0
		for i := s.BoundaryCount() - 1; i >= 0; i-- {
			if s.BoundaryAt(i) <= prior.CompactedAtSeq {
				break
			}
			completedSinceLast++
		}
		if completedSinceLast < cfg.MinTurnGap {
			return false
		}
	}
	// Turn-count limb.
	if cfg.MaxTurns > 0 && s.BoundaryCount() > cfg.MaxTurns {
		return true
	}
	// Token-budget limb. TokenBudgetRatio rides through every
	// resolve layer but stays unused at the predicate level
	// until a MaxPromptTokens accessor lands on pkg/model — see
	// [Config.TokenBudgetRatio] and the spec note in extension.go.
	if cfg.MaxTokens > 0 && s.EstimatedPromptTokens() > cfg.MaxTokens {
		return true
	}
	return false
}
