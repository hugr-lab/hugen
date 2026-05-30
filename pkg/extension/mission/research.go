package mission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// researchWaveLabelPrefix is the wave-label prefix used by the
// runtime's research stage. The runtime treats research-stage
// waves identically to planner waves at the executor level — they
// just dispatch a single subagent and don't run a checker.
const researchWaveLabelPrefix = "_research-"

// runResearchStage runs the pre-planner research pass. Returns
// (aborted, nil) on clean completion: aborted=true means the role
// reported the mission infeasible (status:error) or produced no
// valid handoff within the shape-glitch retry budget, and the
// caller should treat the mission as not viable. (false, nil) means
// findings + resolved_user_inputs (+ optional ac_proposals) are on
// MissionState and the planner can run.
//
// The research role runs ONCE: it does its own read-only discovery
// and asks the user directly via session:inquire when it hits a
// genuine ambiguity, then emits one terminal kind=research handoff.
// The runtime does NOT drive a clarification re-fire loop — the
// only re-spawn here is a bounded retry when the handoff comes back
// malformed (wrong kind / undecodable), capped at
// researchValidationRetryCap.
func (e *Extension) runResearchStage(ctx context.Context, executor *Executor, mission extension.SessionState, manifest MissionManifest, missionSkill, goal string) (bool, error) {
	// Presence of the research block IS the gate (the `when`
	// predicate was removed — see MissionResearchBlock). When a skill
	// declares a researcher role, the stage runs on every mission;
	// skills that never need research omit the block.
	if manifest.Research == nil {
		return false, nil
	}

	m := FromState(mission)
	if m == nil {
		return true, errors.New("mission: research: no MissionState on session")
	}
	// Flip the attempted bit early — even an immediate abort still
	// counts as "research ran" so callGetResearch can disambiguate
	// "no research configured" from "tried and failed".
	m.MarkResearchAttempted()

	// The research role runs ONCE: it does its own discovery and asks
	// the user directly (session:inquire) when it hits an ambiguity,
	// then emits one terminal kind=research handoff with findings.
	// There is no clarification re-fire loop. The bounded loop here
	// is purely a shape-glitch retry budget — a wrong-kind or
	// undecodable handoff gets validationFeedback and one more
	// attempt, capped at researchValidationRetryCap.
	var validationFeedback []string
	maxAttempts := researchValidationRetryCap + 1
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		e.emitResearchIteration(mission, attempt, maxAttempts)

		task, taskErr := buildResearchTask(mission, manifest, goal, validationFeedback)
		if taskErr != nil {
			return true, fmt.Errorf("mission: research: build task: %w", taskErr)
		}
		waveLabel := researchWaveLabelPrefix + strconv.Itoa(attempt)
		wave := Wave{
			Label:     waveLabel,
			SkipCheck: true,
			Subagents: []SubagentSpec{{
				Name:  "researcher",
				Skill: missionSkill,
				Role:  manifest.Research.Role,
				Task:  task,
			}},
		}
		status, _, runErr := executor.RunWave(ctx, mission, wave, RunWaveOptions{})
		e.emitWaveComplete(mission, waveLabel, status, runErr)
		if runErr != nil {
			return true, fmt.Errorf("mission: research: wave run: %w", runErr)
		}
		if status == WaveStatusFailed {
			return true, errors.New("mission: research: wave failed")
		}

		ref, refErr := MakeRef("researcher", waveLabel)
		if refErr != nil {
			return true, fmt.Errorf("mission: research: ref: %w", refErr)
		}
		h, ok := m.Handoffs.Get(ref)
		if !ok {
			return true, fmt.Errorf("mission: research: no handoff under %q", ref)
		}
		if h.Kind != KindResearch {
			if attempt >= maxAttempts {
				return true, fmt.Errorf("mission: research: handoff kind=%q (not research) after %d attempts", h.Kind, attempt)
			}
			validationFeedback = append(validationFeedback, fmt.Sprintf("expected kind=research handoff, got kind=%q. Emit a fenced ```research``` block.", h.Kind))
			continue
		}
		if h.Status != "ok" {
			return true, fmt.Errorf("mission: research: handoff status=%q reason=%q", h.Status, h.Reason)
		}

		out, decodeErr := DecodeResearchOutput(h)
		if decodeErr != nil {
			if attempt >= maxAttempts {
				return true, fmt.Errorf("mission: research: decode: %w (after %d attempts)", decodeErr, attempt)
			}
			validationFeedback = append(validationFeedback, fmt.Sprintf("research handoff failed to decode: %s. Re-emit a valid `research` fenced block matching the contract.", decodeErr.Error()))
			continue
		}

		if strings.TrimSpace(out.MemorySummary) != "" {
			m.PlanContext.Append(PlanContextEntry{
				Iteration: 0, // research runs BEFORE iteration 1
				Phase:     "research",
				Role:      manifest.Research.Role,
				Name:      "researcher",
				Wave:      waveLabel,
				Summary:   out.MemorySummary,
			})
		}
		m.SetResearchOutput(strings.TrimSpace(out.Findings), out.ResolvedUserInputs, out.ACProposals)
		e.emitResearchComplete(mission, attempt, out)
		return false, nil
	}
	return true, errors.New("mission: research: exhausted retry budget without a valid handoff")
}

// researchValidationRetryCap is the shape-glitch retry budget for
// the single research pass. Two retries are enough for a
// recoverable wrong-kind / undecodable-handoff glitch; a third
// failure means the role is broken and the mission aborts.
const researchValidationRetryCap = 2

// buildResearchTask renders the research role's first-message
// task — the user goal + caller-supplied spawn inputs (+ a
// shape-glitch retry note when the prior handoff was malformed).
func buildResearchTask(mission extension.SessionState, manifest MissionManifest, goal string, validationFeedback []string) (string, error) {
	view := researchTaskView{
		Goal:               goal,
		ValidationFeedback: validationFeedback,
	}
	if m := FromState(mission); m != nil {
		// Phase 5.x-followup — caller's spawn-time inputs are
		// authoritative; the research role MUST treat them as
		// already-resolved and skip any clarification it would
		// otherwise ask for those keys. Without this surface the
		// researcher re-prompts for things the caller already
		// passed (file_path, output_format, schedule_kind, …).
		view.SpawnInputs = projectResolvedInputsForTemplate(m.SpawnInputs())
	}
	renderer := mission.Prompts()
	if renderer == nil {
		return "", fmt.Errorf("mission: research task: no prompts renderer on session")
	}
	return renderer.Render("mission/research_task", view)
}

// researchTaskView is the typed payload the
// `mission/research_task` template renders against. Kept narrow
// — the role's domain prose lives in the skill's role system
// prompt; this task message carries only the goal, the caller's
// resolved inputs, and a shape-retry note.
type researchTaskView struct {
	Goal               string
	ValidationFeedback []string
	// SpawnInputs lists the structured key/value pairs the caller
	// passed at spawn_mission time. Authoritative — the researcher
	// MUST treat these keys as already resolved and skip any
	// clarification it would otherwise ask for them.
	SpawnInputs []researchKV
}

// researchKV is a sortable (id, value) pair for the template's
// {{range}} blocks. Map ranges are non-deterministic in Go;
// projecting to a slice keeps the rendered prompt stable across
// runs.
type researchKV struct {
	Key   string
	Value string
}

func projectKVForTemplate(m map[string]string) []researchKV {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Lexical sort — stable + cheap; the template doesn't rely on
	// emission order beyond "consistent across runs".
	sort.Strings(keys)
	out := make([]researchKV, 0, len(keys))
	for _, k := range keys {
		out = append(out, researchKV{Key: k, Value: m[k]})
	}
	return out
}

// projectResolvedInputsForTemplate flattens the typed
// map[string]any from MissionState into the sortable researchKV
// slice the planner_task template expects. Values are stringified
// via fmt.Sprintf("%v", ...) — fine for the strings/numbers/bools
// research stages typically resolve; complex shapes (nested maps)
// surface as Go's default formatting, which is rare in this path
// and acceptable as a stop-gap (rich shapes belong in inputs, not
// the prompt-rendered summary).
func projectResolvedInputsForTemplate(m map[string]any) []researchKV {
	if len(m) == 0 {
		return nil
	}
	flat := make(map[string]string, len(m))
	for k, v := range m {
		flat[k] = fmt.Sprintf("%v", v)
	}
	return projectKVForTemplate(flat)
}

// projectACProposalsForTemplate trims the typed ACProposal slice
// down to the planner-visible shape (statement + rationale). The
// `origin_clarification` field is research-internal — planner
// doesn't need it.
func projectACProposalsForTemplate(in []ResearchACProposal) []researchACProposalView {
	if len(in) == 0 {
		return nil
	}
	out := make([]researchACProposalView, 0, len(in))
	for _, p := range in {
		out = append(out, researchACProposalView{
			Statement: strings.TrimSpace(p.Statement),
			Rationale: strings.TrimSpace(p.Rationale),
		})
	}
	return out
}

// emitResearchIteration publishes a research_iteration
// ExtensionFrame on the mission's event log so scenarios + liveview
// can observe the research stage progressing through iterations.
func (e *Extension) emitResearchIteration(mission extension.SessionState, iter, maxIter int) {
	payload := struct {
		Iteration     int `json:"iteration"`
		MaxIterations int `json:"max_iterations"`
	}{
		Iteration:     iter,
		MaxIterations: maxIter,
	}
	e.emitMissionOp(mission, "research_iteration", payload)
}

// emitResearchComplete fires on successful done=true exit; carries
// the resolved-input keys + ac_proposal count so the harness can
// assert on shape without parsing the full handoff.
func (e *Extension) emitResearchComplete(mission extension.SessionState, iter int, out *ResearchOutput) {
	resolvedKeys := make([]string, 0, len(out.ResolvedUserInputs))
	for k := range out.ResolvedUserInputs {
		resolvedKeys = append(resolvedKeys, k)
	}
	payload := struct {
		Iterations        int      `json:"iterations"`
		ResolvedInputKeys []string `json:"resolved_input_keys,omitempty"`
		ACProposals       int      `json:"ac_proposals,omitempty"`
		Findings          string   `json:"findings,omitempty"`
	}{
		Iterations:        iter,
		ResolvedInputKeys: resolvedKeys,
		ACProposals:       len(out.ACProposals),
		Findings:          out.Findings,
	}
	e.emitMissionOp(mission, "research_complete", payload)
}

// emitMissionOp marshals payload to JSON and publishes it on the
// mission session's event log as a mission:<kind> ExtensionFrame
// in CategoryOp. Marshal failures land in the warn log; the loop
// continues — telemetry isn't load-bearing.
func (e *Extension) emitMissionOp(mission extension.SessionState, kind string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		e.logger.Warn("mission: emitMissionOp: marshal failed",
			"mission_session", mission.SessionID(), "kind", kind, "err", err)
		return
	}
	frame := protocol.NewExtensionFrame(
		mission.SessionID(),
		agentParticipant(mission, e.agentID),
		providerName,
		protocol.CategoryOp,
		kind,
		data,
	)
	if err := mission.Emit(context.Background(), frame); err != nil {
		e.logger.Warn("mission: emitMissionOp: emit failed",
			"mission_session", mission.SessionID(), "kind", kind, "err", err)
	}
}
