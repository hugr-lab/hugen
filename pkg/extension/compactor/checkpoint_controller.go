// checkpoint_controller.go — Stage 2 (L3) per-iteration trigger
// evaluation. Implements [extension.ContextController]: the session
// calls EvaluateContext once per model-iteration boundary and the
// compactor (owner of the history + checkpoint state + resolved
// config) decides whether the upcoming tool dispatch should be
// blocked, plus the advisory to inject.
//
// Two triggers, both re-evaluated every iteration (never latched):
//   - trigger 1 (segment window) — the current open segment's local
//     estimate crossed CheckpointWindowTokens → checkpoint_required.
//   - trigger 2 (budget band) — the real prompt occupancy crossed
//     ContextHideRatio × budget → context_full.
//
// Root-off / disabled is handled here: a depth-0 root session, or a
// session whose resolved config disables checkpoints, gets a zero
// decision (no blocks, no inject).
package compactor

import (
	"context"
	"strings"

	"github.com/hugr-lab/hugen/pkg/extension"
)

// EvaluateContext implements [extension.ContextController].
func (e *Extension) EvaluateContext(ctx context.Context, state extension.SessionState, in extension.ContextInput) extension.ContextDecision {
	s := FromState(state)
	if s == nil {
		return extension.ContextDecision{}
	}
	cfg := e.resolveTierConfig(ctx, state)
	// Default subagents-on / root-off (§6.8): the mechanism only arms
	// on a spawned session. A depth-0 root never blocks — its growth is
	// the turn-boundary compactor's job — until the root↔compactor
	// composition rule lands (§9).
	if !cfg.CheckpointsEnabled || state.Depth() == 0 {
		return extension.ContextDecision{}
	}
	window := cfg.CheckpointWindowTokens
	if window <= 0 {
		window = defaultCheckpointWindowTokens
	}
	hideRatio := cfg.ContextHideRatio
	if hideRatio <= 0 {
		hideRatio = defaultContextHideRatio
	}

	dec := extension.ContextDecision{}

	// Trigger 1 — segment window (local estimate, hide-immune).
	seg := s.SegmentTokens()
	if seg > window {
		dec.CheckpointRequired = true
	}

	// Trigger 2 — budget band (real occupancy). Inert without a tier
	// budget or before the first usage report. hideThreshold is computed
	// regardless (0 when no budget) so it can be surfaced to the model.
	hideThreshold := 0
	if in.Budget > 0 {
		hideThreshold = int(hideRatio * float64(in.Budget))
		if in.RealPromptTokens > 0 && in.RealPromptTokens >= hideThreshold {
			dec.ContextFull = true
		}
	}

	// Stamp the occupancy so the context:* tool results can show the
	// model how full its context is (it has no token counter of its own
	// — without this it hides blind).
	s.SetOccupancy(in.RealPromptTokens, in.Budget, hideThreshold)

	// Advisory — context_full (shedding) supersedes the softer
	// checkpoint nudge when both fire.
	switch {
	case dec.ContextFull:
		dec.Inject = e.renderContextFullAdvisory(s, state, in)
	case dec.CheckpointRequired:
		dec.Inject = e.renderCheckpointNudge(s, state, in, seg, window, hideThreshold)
	}
	return dec
}

// checkpointNudgeInput binds `assets/prompts/compactor/checkpoint_nudge.tmpl`.
type checkpointNudgeInput struct {
	SegmentK  int
	WindowK   int
	Fill      string // metric line; "" when occupancy unknown
	NearBand  bool
	HasClosed bool
}

// cpListItem is one row of the shed menu in the context_full advisory.
type cpListItem struct {
	ID      string
	TokensK int
	Desc    string
}

// contextFullInput binds `assets/prompts/compactor/context_full.tmpl`.
type contextFullInput struct {
	UsedK   int
	BudgetK int
	Closed  []cpListItem
}

// renderCheckpointNudge is the trigger-1 system message: the segment is
// over the window, close it before continuing. It surfaces the real
// context fill so the model only sheds when actually filling — not
// blindly on every checkpoint (the dogfood failure where it hid early
// and lost an instruction). Prose lives in the template; this builds the
// binding. Returns "" with no renderer / on render error (the dispatch
// block still applies via the turn flags).
func (e *Extension) renderCheckpointNudge(s *CompactorState, state extension.SessionState, in extension.ContextInput, seg, window, hideThreshold int) string {
	renderer := state.Prompts()
	if renderer == nil {
		return ""
	}
	out, err := renderer.Render("compactor/checkpoint_nudge", checkpointNudgeInput{
		SegmentK:  kTokens(seg),
		WindowK:   kTokens(window),
		Fill:      fillSummary(in.RealPromptTokens, in.Budget, hideThreshold),
		NearBand:  hideThreshold > 0 && in.RealPromptTokens >= hideThreshold*8/10,
		HasClosed: closedVisibleCount(s) > 0,
	})
	if err != nil {
		e.logger.Warn("compactor: render checkpoint_nudge", "session", state.SessionID(), "err", err)
		return ""
	}
	return strings.TrimSpace(out)
}

// renderContextFullAdvisory is the trigger-2 system message: occupancy
// crossed the hide band; shed context (hide / rollback) to continue.
func (e *Extension) renderContextFullAdvisory(s *CompactorState, state extension.SessionState, in extension.ContextInput) string {
	renderer := state.Prompts()
	if renderer == nil {
		return ""
	}
	out, err := renderer.Render("compactor/context_full", contextFullInput{
		UsedK:   kTokens(in.RealPromptTokens),
		BudgetK: kTokens(in.Budget),
		Closed:  visibleClosedItems(s),
	})
	if err != nil {
		e.logger.Warn("compactor: render context_full", "session", state.SessionID(), "err", err)
		return ""
	}
	return strings.TrimSpace(out)
}

// visibleClosedItems lists the shed-able (closed, not hidden) checkpoints
// as template rows. Hidden ones are omitted — they're already shed.
func visibleClosedItems(s *CompactorState) []cpListItem {
	cps := s.Checkpoints()
	var out []cpListItem
	for _, cp := range cps {
		if cp.Hidden {
			continue
		}
		desc := strings.TrimSpace(cp.Description)
		if desc == "" {
			desc = "(unlabelled)"
		}
		out = append(out, cpListItem{ID: cp.ID, TokensK: kTokens(cp.Tokens), Desc: desc})
	}
	return out
}

// closedVisibleCount counts checkpoints that are currently visible
// (closed but not hidden) — the ones context:hide can still act on.
func closedVisibleCount(s *CompactorState) int {
	cps := s.Checkpoints()
	n := 0
	for _, cp := range cps {
		if !cp.Hidden {
			n++
		}
	}
	return n
}

// kTokens rounds a token count to whole thousands for display, with a
// floor of 1 so a small-but-nonzero figure never reads as "0K".
func kTokens(n int) int {
	if n <= 0 {
		return 0
	}
	k := (n + 500) / 1000
	if k < 1 {
		return 1
	}
	return k
}
