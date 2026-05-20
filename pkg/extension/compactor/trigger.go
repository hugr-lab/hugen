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
func (e *Extension) OnTurnBoundary(ctx context.Context, state extension.SessionState) error {
	if !e.shouldCompact(state) {
		return nil
	}
	if err := e.compact(ctx, state); err != nil {
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
// Per-tier config / per-skill manifest overrides are γ's
// problem; β reads the flat [Config] verbatim.
func (e *Extension) shouldCompact(state extension.SessionState) bool {
	if !e.cfg.Enabled {
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
	if s.BoundaryCount() <= e.cfg.PreservedRecentTurns {
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
		if completedSinceLast < e.cfg.MinTurnGap {
			return false
		}
	}
	// Turn-count limb.
	if e.cfg.MaxTurns > 0 && s.BoundaryCount() > e.cfg.MaxTurns {
		return true
	}
	// Token-budget limb (absolute floor in β; γ adds a ratio
	// against the resolved model's MaxPromptTokens).
	if e.cfg.MaxTokens > 0 && s.EstimatedPromptTokens() > e.cfg.MaxTokens {
		return true
	}
	return false
}
