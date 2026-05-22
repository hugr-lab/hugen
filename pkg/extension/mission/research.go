package mission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// runResearchStage drives the pre-planner research loop. Returns
// (aborted, nil) on clean completion: aborted=true means the role
// either ran out of MaxIterations or emitted an unrecoverable
// error, and the caller should treat the mission as not viable.
// (false, nil) means findings + resolved_user_inputs (+ optional
// ac_proposals) are on MissionState and the planner can run.
//
// The loop:
//
//   1. Spawn the research role as a single-subagent wave.
//   2. Decode the handoff as kind=research.
//   3. If output.Done — stash findings on MissionState, return.
//   4. Else — batch the clarifications into a single
//      session:inquire modal, collect answers, fold them into
//      `prior_answers` + `prior_comments` for the next research
//      iteration's first message.
//   5. Cap at manifest.Research.MaxIterations.
func (e *Extension) runResearchStage(ctx context.Context, executor *Executor, mission extension.SessionState, manifest MissionManifest, missionSkill, goal string) (bool, error) {
	if manifest.Research == nil {
		return false, nil
	}
	if !shouldRunResearch(manifest, goal) {
		e.logger.Info("mission: research stage skipped by trigger predicate",
			"mission_session", mission.SessionID(),
			"when", manifest.Research.When,
			"role", manifest.Research.Role)
		return false, nil
	}
	maxIter := manifest.Research.MaxIterations
	if maxIter <= 0 {
		maxIter = ResearchDefaultMaxIterations
	}
	if maxIter > ResearchMaxIterationsCap {
		maxIter = ResearchMaxIterationsCap
	}

	m := FromState(mission)
	if m == nil {
		return true, errors.New("mission: research: no MissionState on session")
	}

	var priorAnswers map[string]string
	var priorComments map[string]string
	var validationFeedback []string

	for iter := 1; iter <= maxIter; iter++ {
		e.emitResearchIteration(mission, iter, maxIter)

		task, taskErr := buildResearchTask(mission, manifest, goal, iter, priorAnswers, priorComments, validationFeedback)
		if taskErr != nil {
			return true, fmt.Errorf("mission: research: build task: %w", taskErr)
		}
		waveLabel := researchWaveLabelPrefix + strconv.Itoa(iter)
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
			return true, fmt.Errorf("mission: research iter %d: wave run: %w", iter, runErr)
		}
		if status == WaveStatusFailed {
			return true, fmt.Errorf("mission: research iter %d: wave failed", iter)
		}

		ref, refErr := MakeRef("researcher", waveLabel)
		if refErr != nil {
			return true, fmt.Errorf("mission: research iter %d: ref: %w", iter, refErr)
		}
		h, ok := m.Handoffs.Get(ref)
		if !ok {
			return true, fmt.Errorf("mission: research iter %d: no handoff under %q", iter, ref)
		}
		if h.Kind != KindResearch {
			// Researcher emitted a different kind — surface and
			// retry once via validation feedback. After 2 retries
			// (matching planner pattern) we abort. We reuse the
			// validationFeedback path so the role sees the same
			// re-fire cue as for schema failures.
			validationFeedback = append(validationFeedback, fmt.Sprintf("expected kind=research handoff, got kind=%q. Emit a fenced ```research``` block.", h.Kind))
			if len(validationFeedback) >= researchValidationRetryCap {
				return true, fmt.Errorf("mission: research iter %d: handoff kind=%q (not research) after %d retries", iter, h.Kind, len(validationFeedback))
			}
			// Don't count this iter against MaxIterations — the role
			// hasn't actually emitted a valid research output yet.
			iter--
			maxIter--
			continue
		}
		if h.Status != "ok" {
			return true, fmt.Errorf("mission: research iter %d: handoff status=%q reason=%q", iter, h.Status, h.Reason)
		}
		validationFeedback = nil

		out, decodeErr := DecodeResearchOutput(h)
		if decodeErr != nil {
			return true, fmt.Errorf("mission: research iter %d: decode: %w", iter, decodeErr)
		}

		// Findings + memory_summary always flow to plan_context so
		// the planner sees the latest research read even on a
		// done=false iteration (the planner can use mid-research
		// state if it ever runs in parallel — today it doesn't, but
		// the journal is append-only by design).
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

		if out.Done {
			m.SetResearchOutput(strings.TrimSpace(out.Findings), out.ResolvedUserInputs, out.ACProposals)
			e.emitResearchComplete(mission, iter, out)
			return false, nil
		}

		if len(out.Clarifications) == 0 {
			// done=false with no clarifications — output_contract
			// validation already rejects this, but defensive guard
			// here keeps the loop honest.
			return true, fmt.Errorf("mission: research iter %d: done=false with no clarifications", iter)
		}

		answers, comments, inqErr := e.batchedInquire(ctx, mission, out.Clarifications)
		if inqErr != nil {
			return true, fmt.Errorf("mission: research iter %d: inquire: %w", iter, inqErr)
		}
		priorAnswers = answers
		priorComments = comments

		if iter == maxIter {
			return true, fmt.Errorf("mission: research: exceeded MaxIterations=%d without done=true", maxIter)
		}
	}
	return true, errors.New("mission: research: loop exit without resolution")
}

// researchValidationRetryCap matches the planner's validator
// retry budget (Phase I.20+). Two retries are enough for a
// recoverable shape glitch; three failed turns mean the role is
// broken.
const researchValidationRetryCap = 2

// batchedInquire dispatches the research role's clarifications as
// one InquiryRequest on the mission session and returns the
// per-id answers + per-id comments.
//
// Returns:
//   - (answers, comments, nil) on a successful round-trip.
//   - (nil, nil, err) on emit failure, timeout, or empty user
//     reply (treated as decline — research aborts).
//
// The mission session inquires on its own behalf; the parent
// chain bubbles the request up to root and back. The pump
// re-keys frames as usual.
func (e *Extension) batchedInquire(ctx context.Context, mission extension.SessionState, clarifications []ResearchClarification) (map[string]string, map[string]string, error) {
	proto := make([]protocol.Clarification, 0, len(clarifications))
	for _, c := range clarifications {
		proto = append(proto, protocol.Clarification{
			ID:           c.ID,
			Question:     c.Question,
			Kind:         c.Kind,
			Options:      append([]string(nil), c.Options...),
			Default:      c.Default,
			AllowComment: c.AllowComment,
		})
	}
	payload := protocol.InquiryRequestPayload{
		Type:           protocol.InquiryTypeResearchBatch,
		Clarifications: proto,
	}
	resp, err := mission.RequestInquiry(ctx, payload)
	if err != nil {
		return nil, nil, err
	}
	if resp == nil {
		return nil, nil, errors.New("nil response from inquire")
	}
	if resp.Payload.Timeout {
		return nil, nil, errors.New("research inquire timed out")
	}
	answers := make(map[string]string, len(clarifications))
	comments := make(map[string]string, len(clarifications))
	// Prefer the typed answers map. Fall back to splitting the
	// flat Response string on newlines for adapters that haven't
	// learned the new shape yet (treats each line as the next
	// clarification's value in order).
	if len(resp.Payload.Answers) > 0 {
		for id, entry := range resp.Payload.Answers {
			if entry.Value != "" {
				answers[id] = entry.Value
			}
			if entry.Comment != "" {
				comments[id] = entry.Comment
			}
		}
	} else if free := strings.TrimSpace(resp.Payload.Response); free != "" {
		lines := strings.Split(free, "\n")
		for i, line := range lines {
			if i >= len(clarifications) {
				break
			}
			answers[clarifications[i].ID] = strings.TrimSpace(line)
		}
	}
	return answers, comments, nil
}

// shouldRunResearch decides whether the research stage fires for
// this mission. Centralises the When predicate so callers (mainly
// runResearchStage but also future planner-side checks) share one
// rule.
//
// Predicate kinds:
//
//   - `always` — fires unconditionally.
//   - `auto`   — runtime heuristic (see autoResearchHeuristic).
//   - `if_goal_matches` — Predicate regex against goal text.
//
// Empty / unknown When values default to skipping research — the
// projection layer normalises these at load time so a manifest
// that reaches here with an unknown When is a runtime bug.
func shouldRunResearch(manifest MissionManifest, goal string) bool {
	if manifest.Research == nil {
		return false
	}
	switch manifest.Research.When {
	case ResearchWhenAlways:
		return true
	case ResearchWhenAuto:
		return autoResearchHeuristic(goal, manifest)
	case ResearchWhenIfGoalMatches:
		return matchGoalPredicate(manifest.Research.Predicate, goal)
	default:
		return false
	}
}

// buildResearchTask renders the research role's first-message
// task. Carries the user goal + (on re-fire iterations) the
// prior batch's structured answers and free-form comments so the
// role can incorporate them.
func buildResearchTask(mission extension.SessionState, manifest MissionManifest, goal string, iteration int, priorAnswers, priorComments map[string]string, validationFeedback []string) (string, error) {
	view := researchTaskView{
		Goal:               goal,
		Iteration:          iteration,
		MaxIterations:      manifest.Research.MaxIterations,
		PriorAnswers:       projectKVForTemplate(priorAnswers),
		PriorComments:      projectKVForTemplate(priorComments),
		ValidationFeedback: validationFeedback,
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
// prompt; this task message only carries iteration metadata +
// prior-turn answers.
type researchTaskView struct {
	Goal               string
	Iteration          int
	MaxIterations      int
	PriorAnswers       []researchKV
	PriorComments      []researchKV
	ValidationFeedback []string
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
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
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
