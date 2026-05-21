package stuckdetector

import (
	"context"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// evaluate runs every rising-edge detector against the snapshot
// view of recentHashes. Each detector flips its own active flag
// independently; multiple nudges can fire on the same observation
// if two patterns trip together.
func (e *Extension) evaluate(ctx context.Context, state extension.SessionState, s *DetectorState) {
	if e.disabledByPolicy(ctx, state) {
		return
	}
	samples := s.snapshot()
	e.evaluateRepeatedHash(ctx, state, s, samples)
	e.evaluateTightDensity(ctx, state, s, samples)
	e.evaluateRepeatedError(ctx, state, s, samples)
	e.evaluateNoProgress(ctx, state, s, samples)
}

// evaluateRepeatedHash fires once when the last N = stuckRepeatedHashWindow
// hashes are all equal. The flag clears the moment a different
// hash appears in the tail, allowing later recurrence to fire
// again (spec §13.2 #6).
func (e *Extension) evaluateRepeatedHash(ctx context.Context, state extension.SessionState, s *DetectorState, samples []hashSample) {
	N := stuckRepeatedHashWindow
	if len(samples) < N {
		s.setRepeatedHashActive(false)
		return
	}
	tail := samples[len(samples)-N:]
	first := tail[0].hash
	if first == "" {
		s.setRepeatedHashActive(false)
		return
	}
	for _, ent := range tail[1:] {
		if ent.hash != first {
			s.setRepeatedHashActive(false)
			return
		}
	}
	if !s.setRepeatedHashActive(true) {
		return // pattern continues; already nudged.
	}
	e.emitStuckNudge(ctx, state, "interrupts/stuck_repeated_tool", map[string]any{"N": N})
}

// evaluateTightDensity fires once when M = stuckTightDensityCount
// same-hash calls land within W = stuckTightDensityWindow.
// Different from repeated_hash in that the calls only need to
// share a hash and cluster in time.
func (e *Extension) evaluateTightDensity(ctx context.Context, state extension.SessionState, s *DetectorState, samples []hashSample) {
	M := stuckTightDensityCount
	if len(samples) < M {
		s.setTightDensityActive(false)
		return
	}
	tail := samples[len(samples)-M:]
	first := tail[0].hash
	if first == "" {
		s.setTightDensityActive(false)
		return
	}
	for _, ent := range tail[1:] {
		if ent.hash != first {
			s.setTightDensityActive(false)
			return
		}
	}
	span := tail[len(tail)-1].at.Sub(tail[0].at)
	if span > stuckTightDensityWindow {
		s.setTightDensityActive(false)
		return
	}
	if !s.setTightDensityActive(true) {
		return
	}
	e.emitStuckNudge(ctx, state, "interrupts/stuck_tight_density", map[string]any{
		"M":      M,
		"Window": stuckTightDensityWindow.String(),
	})
}

// evaluateRepeatedError fires once when K = stuckRepeatedErrorCount
// samples inside the trailing W = stuckRepeatedErrorWindow share
// the same (tool, errCode) and at least one of them is the most
// recent sample. CLUSTER count (not strictly consecutive) so the
// alt-pattern (failed call / different call / failed call …)
// catches.
func (e *Extension) evaluateRepeatedError(ctx context.Context, state extension.SessionState, s *DetectorState, samples []hashSample) {
	K := stuckRepeatedErrorCount
	if len(samples) < K {
		s.setRepeatedErrorActive(false)
		return
	}
	last := samples[len(samples)-1]
	if last.errCode == "" {
		// Latest sample succeeded — pattern broken; re-arm.
		s.setRepeatedErrorActive(false)
		return
	}
	window := samples
	if len(window) > stuckRepeatedErrorWindow {
		window = window[len(window)-stuckRepeatedErrorWindow:]
	}
	matches := 0
	for _, ent := range window {
		if ent.tool == last.tool && ent.errCode == last.errCode {
			matches++
		}
	}
	if matches < K {
		s.setRepeatedErrorActive(false)
		return
	}
	if !s.setRepeatedErrorActive(true) {
		return
	}
	e.emitStuckNudge(ctx, state, "interrupts/stuck_repeated_error", map[string]any{
		"K":    K,
		"Tool": last.tool,
		"Code": last.errCode,
	})
}

// evaluateNoProgress fires (as a system_marker, not a
// system_message — per spec §8.3 the no_progress signal surfaces
// via adapter only) when the latest hash matches a prior hash
// AND the prior tool_result was an error. Doesn't break the loop;
// just lights the runway.
func (e *Extension) evaluateNoProgress(ctx context.Context, state extension.SessionState, s *DetectorState, samples []hashSample) {
	if len(samples) < 2 {
		s.setNoProgressActive(false)
		return
	}
	last := samples[len(samples)-1]
	hit := false
	for i := len(samples) - 2; i >= 0; i-- {
		prev := samples[i]
		if prev.hash == last.hash && prev.errCode != "" {
			hit = true
			break
		}
	}
	if !hit {
		s.setNoProgressActive(false)
		return
	}
	if !s.setNoProgressActive(true) {
		return
	}
	e.emitNoProgressMarker(ctx, state, last.hash)
}

// disabledByPolicy folds every [extension.ToolPolicyAdvisor]'s
// DisableStuckNudges into a single bool. Mirrors
// `pkg/session/session.go::gatherToolPolicy`; replicated here so
// the detector doesn't depend on Session internals.
func (e *Extension) disabledByPolicy(ctx context.Context, state extension.SessionState) bool {
	for _, ext := range state.Extensions() {
		advisor, ok := ext.(extension.ToolPolicyAdvisor)
		if !ok {
			continue
		}
		if advisor.AdviseToolPolicy(ctx, state).DisableStuckNudges {
			return true
		}
	}
	return false
}

// emitStuckNudge renders one `interrupts/<template>` body and
// emits a SystemMessage{stuck_nudge} carrying it. The compactor's
// FrameObserver folds the frame into the owned history cache on
// the next emit hop.
func (e *Extension) emitStuckNudge(ctx context.Context, state extension.SessionState, tmpl string, data map[string]any) {
	renderer := state.Prompts()
	if renderer == nil {
		if e.logger != nil {
			e.logger.Warn("stuckdetector: nil prompts renderer; skipping nudge emit",
				"session", state.SessionID(), "template", tmpl)
		}
		return
	}
	content := renderer.MustRender(tmpl, data)
	frame := protocol.NewSystemMessage(
		state.SessionID(),
		protocol.ParticipantInfo{Kind: protocol.ParticipantAgent},
		protocol.SystemMessageStuckNudge,
		content,
	)
	if err := state.Emit(ctx, frame); err != nil && e.logger != nil {
		e.logger.Warn("stuckdetector: emit stuck_nudge",
			"session", state.SessionID(), "template", tmpl, "err", err)
	}
}

// emitNoProgressMarker emits the adapter-only system_marker for
// the no_progress detector. Distinct from emitStuckNudge —
// no_progress is an audit signal, not a model-visible nudge.
func (e *Extension) emitNoProgressMarker(ctx context.Context, state extension.SessionState, hash string) {
	mk := protocol.NewSystemMarker(
		state.SessionID(),
		protocol.ParticipantInfo{Kind: protocol.ParticipantAgent},
		protocol.SubjectNoProgress,
		map[string]any{"hash": hash},
	)
	if err := state.Emit(ctx, mk); err != nil && e.logger != nil {
		e.logger.Warn("stuckdetector: emit no_progress marker",
			"session", state.SessionID(), "err", err)
	}
}
