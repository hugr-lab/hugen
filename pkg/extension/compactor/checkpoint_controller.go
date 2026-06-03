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
	"fmt"
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
		dec.Inject = e.renderContextFullAdvisory(s, in)
	case dec.CheckpointRequired:
		dec.Inject = e.renderCheckpointNudge(s, in, seg, window, hideThreshold)
	}
	return dec
}

// renderCheckpointNudge is the trigger-1 system message: the segment is
// over the window, close it before continuing. It surfaces the real
// context fill so the model only sheds when actually filling — not
// blindly on every checkpoint (the dogfood failure where it hid early
// and lost an instruction).
func (e *Extension) renderCheckpointNudge(s *CompactorState, in extension.ContextInput, seg, window, hideThreshold int) string {
	var b strings.Builder
	fmt.Fprintf(&b,
		"Context note: the current work segment is ~%dK tokens, over the ~%dK window. "+
			"Call context:checkpoint(description=\"…\") to close it before any further tool calls — "+
			"tool calls are blocked until you do.",
		kTokens(seg), kTokens(window))
	if fill := fillSummary(in.RealPromptTokens, in.Budget, hideThreshold); fill != "" {
		fmt.Fprintf(&b, " %s.", fill)
		// Suggest shedding only within ~80% of the band, and only if
		// there is a closed segment to shed; otherwise reassure there is
		// headroom so the model doesn't hide prematurely.
		nearBand := hideThreshold > 0 && in.RealPromptTokens >= hideThreshold*8/10
		if nearBand && closedVisibleCount(s) > 0 {
			b.WriteString(" Context is getting full — consider context:hide(cp_id) on an earlier closed segment (its detail is summarised into a placeholder, so the takeaway survives).")
		} else {
			b.WriteString(" Plenty of headroom — no need to hide yet; just keep checkpointing.")
		}
	}
	return b.String()
}

// renderContextFullAdvisory is the trigger-2 system message: occupancy
// crossed the hide band; shed context (hide / rollback) to continue.
func (e *Extension) renderContextFullAdvisory(s *CompactorState, in extension.ContextInput) string {
	var b strings.Builder
	fmt.Fprintf(&b,
		"Context is filling: prompt is ~%dK of the ~%dK budget, over the shed band. "+
			"Tool calls are blocked until you free context.",
		kTokens(in.RealPromptTokens), kTokens(in.Budget))
	list := renderCheckpointList(s)
	if list != "" {
		b.WriteString("\nClosed segments you can shed:\n")
		b.WriteString(list)
		b.WriteString("\nCall context:hide(cp_id=\"cp-N\") to collapse a closed segment, " +
			"or context:rollback(cp_id=\"cp-N\", note=\"…\") to drop a bad branch wholesale.")
	} else {
		b.WriteString(" No closed segments yet — call context:checkpoint(description=\"…\") to " +
			"close the current work so it becomes hideable, then context:hide it.")
	}
	return b.String()
}

// renderCheckpointList renders the visible closed segments as a compact
// menu (hidden ones are omitted — they're already shed). Empty when
// there is nothing to hide.
func renderCheckpointList(s *CompactorState) string {
	cps := s.Checkpoints()
	var b strings.Builder
	for _, cp := range cps {
		if cp.Hidden {
			continue
		}
		desc := strings.TrimSpace(cp.Description)
		if desc == "" {
			desc = "(unlabelled)"
		}
		fmt.Fprintf(&b, "  - %s (~%dK tok): %s\n", cp.ID, kTokens(cp.Tokens), desc)
	}
	return strings.TrimRight(b.String(), "\n")
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
