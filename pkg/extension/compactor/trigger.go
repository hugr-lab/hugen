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
// Phase α (this commit): the predicate always returns false so
// no dispatch happens. Future milestones populate this:
//
//   - β wires the predicate's token + turn limbs + the
//     compaction LLM call.
//   - γ adds per-tier config resolution + skill-manifest opt-in.
//   - δ extends scenario coverage + the UI marker.
func (e *Extension) OnTurnBoundary(ctx context.Context, state extension.SessionState) error {
	if !e.shouldCompact(state) {
		return nil
	}
	// Compaction pipeline lands in β.
	return nil
}

// shouldCompact is the hybrid trigger predicate. Phase α
// returns false unconditionally — the structural wiring is in
// place but no compaction fires until β + γ flesh out the
// per-tier config + LLM pipeline.
//
// β implements the real shape (spec §4.2):
//
//   - skip if !cfg.Enabled (per-tier resolved)
//   - skip if BoundaryCount <= cfg.PreservedRecentTurns
//   - turn-count limb: fire if BoundaryCount > cfg.MaxTurns
//   - token-budget limb: fire if EstimatedPromptTokens >
//     cfg.TokenBudgetRatio * intent.MaxPromptTokens
//   - min_turn_gap anti-thrash gate
//   - blocked by active tool feed / cancel / recoverable
//     prior-turn error
func (e *Extension) shouldCompact(state extension.SessionState) bool {
	if !e.cfg.Enabled {
		return false
	}
	// β implementation: pull tier-resolved cfg via skill manifest +
	// runtime defaults; consult state.CompactorState.
	_ = state
	return false
}
