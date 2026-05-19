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

// plannerWaveLabelPrefix is the per-iteration wave label mission
// ext uses when spawning a planner subagent. Underscore-prefixed so
// it sorts apart from operator-authored wave labels in PlanState.Done
// and won't collide with the planner's own next_wave.label output
// (which is operator-controlled). The full label is
// "_plan-<iteration>" — 1-indexed for human readability in event
// logs / liveview status.
const plannerWaveLabelPrefix = "_plan-"

// checkerWaveLabelPrefix mirrors plannerWaveLabelPrefix for the
// verdict phase. Full label: "_check-<iteration>".
const checkerWaveLabelPrefix = "_check-"

// PlannerError flags a non-recoverable failure inside the planner
// loop — a planner wave that never produced a handoff, an output
// that failed validation past the retry cap, or an approval
// requirement that the planner ignored. Wraps the underlying cause
// so callers can errors.As / errors.Is against ParseError when
// they want to distinguish schema violations from infrastructure
// failures.
type PlannerError struct {
	Iteration int
	Reason    string
	Err       error
}

func (e *PlannerError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err == nil {
		return fmt.Sprintf("planner iteration %d: %s", e.Iteration, e.Reason)
	}
	return fmt.Sprintf("planner iteration %d: %s: %v", e.Iteration, e.Reason, e.Err)
}

func (e *PlannerError) Unwrap() error { return e.Err }

// driveMissionPlanner is the Phase-B mission driver. Spawns the
// planner LLM subagent, runs its emitted wave, re-spawns the
// planner with the wave's outcome, repeats until either the
// planner signals plan_complete (next_wave: null) or the manifest's
// max_waves cap is hit.
//
// On any planner-error / wave-failure the loop aborts and falls
// through to synthesis (with aborted=true), so the mission still
// produces a recap message even on infrastructure failure. The
// planner's per-iteration outcome is recorded as a regular
// PlanState.Done entry so liveview / event-log observability
// matches the inline path.
//
// Returns when the mission session has emitted its final
// AgentMessage; the caller (RunMission) drives nothing further.
func (e *Extension) driveMissionPlanner(mission extension.SessionState, spawner extension.SessionSpawner, manifest MissionManifest, missionSkill, goal string, inputs any) {
	executor := NewExecutor(e.makeSpawnerCallback(mission, spawner), e.logger)
	ctx := context.Background()
	_ = inputs

	aborted, err := e.runPlannerLoop(ctx, executor, mission, manifest, missionSkill, goal)
	if err != nil {
		e.logger.Warn("mission: driveMissionPlanner: planner loop failed",
			"mission_session", mission.SessionID(),
			"err", err)
	}

	var synthText string
	if !aborted && manifest.Synthesis.Role != "" && missionHasHandoffs(mission) {
		text, synthErr := e.runSynthesis(ctx, executor, mission, manifest.Synthesis.Role, missionSkill, goal)
		if synthErr != nil {
			e.logger.Warn("mission: driveMissionPlanner: synthesis failed",
				"mission_session", mission.SessionID(), "err", synthErr)
		} else {
			synthText = text
		}
	}

	final := buildFinalText(mission, synthText, aborted)
	e.finishMission(ctx, mission, final)
}

// runPlannerLoop is the iterative driver carved out of
// driveMissionPlanner for unit-testability. Returns (aborted,
// reason) — aborted=true means synthesis should still run on
// whatever partial state was collected but the recap will reflect
// failure.
//
// The loop invariants:
//   - At most manifest.Plan.MaxWaves wave-iterations run; the
//     planner itself spawns are not capped separately (each plan
//     iteration is one planner spawn + one executed wave).
//   - The first plan handoff with next_wave=null exits the loop
//     cleanly (aborted=false).
//   - Any wave failure, planner handoff parse failure, or context
//     cancellation aborts the loop (aborted=true).
func (e *Extension) runPlannerLoop(ctx context.Context, executor *Executor, mission extension.SessionState, manifest MissionManifest, missionSkill, goal string) (bool, error) {
	maxIter := manifest.Plan.MaxWaves
	if maxIter <= 0 {
		maxIter = DefaultMaxWaves
	}

	// recentVerdict carries the most recent checker verdict into
	// the next planner spawn's first message. Empty on iteration 1
	// (no prior wave); populated by every subsequent iteration when
	// a control role is declared. Phase C.
	var recentVerdict *Verdict

	for iteration := 1; iteration <= maxIter; iteration++ {
		// Stamp the iteration on MissionState so ingestHandoff
		// tags plan_context entries with the right number even
		// when the handoff arrives between iterations.
		if m := FromState(mission); m != nil {
			m.mu.Lock()
			m.IterationCounter = iteration
			m.mu.Unlock()
		}
		e.emitIterationStart(mission, iteration, manifest.Plan.MaxWaves)

		// 1. Spawn the planner subagent.
		plan, plannerErr := e.spawnAndAwaitPlanner(ctx, executor, mission, manifest, missionSkill, goal, iteration, recentVerdict)
		if plannerErr != nil {
			return true, plannerErr
		}

		// 2. plan_complete signal — no more waves to run.
		if plan == nil {
			e.logger.Info("mission: planner loop: plan_complete received",
				"mission_session", mission.SessionID(),
				"iteration", iteration)
			return false, nil
		}

		// 3. Run the planner-emitted wave. Each subagent's task is
		// decorated with the runtime-injected handoff contract so
		// the worker knows to end its turn with a fenced handoff
		// block — without this most weak models go off and start
		// executing the task literally (calling bash, hanging on
		// approval) instead of emitting a result fence.
		decorated, decorateErr := decorateWaveTasks(mission, plan.NextWave)
		if decorateErr != nil {
			e.logger.Warn("mission: planner loop: decorate wave tasks failed",
				"mission_session", mission.SessionID(), "iteration", iteration, "err", decorateErr)
			return true, decorateErr
		}
		status, _, runErr := executor.RunWave(ctx, mission, decorated, RunWaveOptions{})
		e.emitWaveComplete(mission, plan.NextWave.Label, status, runErr)
		if runErr != nil || status == WaveStatusFailed {
			e.logger.Warn("mission: planner loop: executed wave failed",
				"mission_session", mission.SessionID(),
				"iteration", iteration,
				"wave", plan.NextWave.Label,
				"status", status,
				"err", runErr)
			return true, runErr
		}

		// 4. Phase C — verdict phase. Spawn checker if declared,
		// route on its decision. Absent control collapses to the
		// implicit `continue` path.
		recentVerdict = nil
		if manifest.Control.Role != "" {
			verdict, checkErr := e.spawnAndAwaitChecker(ctx, executor, mission, manifest, missionSkill, goal, iteration)
			if checkErr != nil {
				e.logger.Warn("mission: planner loop: checker failed",
					"mission_session", mission.SessionID(),
					"iteration", iteration, "err", checkErr)
				return true, checkErr
			}
			e.emitVerdictReady(mission, iteration, verdict)
			vCopy := verdict
			recentVerdict = &vCopy
			switch verdict.Decision {
			case VerdictContinue, VerdictAmend:
				// fall through to next iteration; amend's Issues
				// ride along via recentVerdict.
			case VerdictInquire:
				// Checker already raised an inquiry inside its
				// turn (validated by spawnAndAwaitChecker). The
				// inquiry bubbled to root and was answered before
				// the checker closed; the next planner sees the
				// verdict's reason in its [Recent verdict] block.
			case VerdictFinish:
				e.logger.Info("mission: planner loop: checker verdict=finish",
					"mission_session", mission.SessionID(),
					"iteration", iteration)
				return false, nil
			}
		}
	}

	// Max-iterations cap hit without plan_complete. Treated as a
	// clean stop (not an abort) — synthesis still runs over whatever
	// handoffs were produced. Spec §0.5: runtime injects a final
	// "wrap up" prompt at the cap; the v1 cut just stops the loop
	// and lets synthesis recap.
	e.logger.Info("mission: planner loop: max_waves cap reached",
		"mission_session", mission.SessionID(),
		"max_waves", maxIter)
	return false, nil
}

// spawnAndAwaitPlanner runs one planner iteration: spawns the
// planner under a `_plan-<N>` wave, waits for its handoff, parses
// it as kind=plan, and decodes the typed Plan AST.
//
// Returns:
//   - (*Plan, nil) when the planner emitted a valid plan with a
//     concrete next_wave.
//   - (nil, nil) when the planner signalled plan_complete
//     (next_wave: null).
//   - (nil, *PlannerError) when the iteration failed (wave error,
//     handoff parse failure, decode failure).
func (e *Extension) spawnAndAwaitPlanner(ctx context.Context, executor *Executor, mission extension.SessionState, manifest MissionManifest, missionSkill, goal string, iteration int, recentVerdict *Verdict) (*Plan, error) {
	if manifest.Plan.Role == "" {
		return nil, &PlannerError{Iteration: iteration, Reason: "manifest.plan.role is empty"}
	}
	task, err := buildPlannerTask(mission, manifest, goal, iteration, recentVerdict)
	if err != nil {
		return nil, &PlannerError{Iteration: iteration, Reason: "build planner task", Err: err}
	}
	waveLabel := plannerWaveLabelPrefix + strconv.Itoa(iteration)
	wave := Wave{
		Label: waveLabel,
		Subagents: []SubagentSpec{{
			Name:  "planner",
			Skill: missionSkill,
			Role:  manifest.Plan.Role,
			Task:  task,
		}},
	}
	status, _, runErr := executor.RunWave(ctx, mission, wave, RunWaveOptions{})
	e.emitWaveComplete(mission, waveLabel, status, runErr)
	if runErr != nil {
		return nil, &PlannerError{Iteration: iteration, Reason: "planner wave run", Err: runErr}
	}
	if status == WaveStatusFailed {
		return nil, &PlannerError{Iteration: iteration, Reason: "planner wave failed"}
	}

	// Recover the planner's handoff.
	ref, refErr := MakeRef("planner", waveLabel)
	if refErr != nil {
		return nil, &PlannerError{Iteration: iteration, Reason: "ref planner@" + waveLabel, Err: refErr}
	}
	m := FromState(mission)
	if m == nil {
		return nil, &PlannerError{Iteration: iteration, Reason: "no MissionState on session"}
	}
	h, ok := m.Handoffs.Get(ref)
	if !ok {
		return nil, &PlannerError{Iteration: iteration, Reason: "no handoff under " + ref}
	}
	if h.Status != "ok" {
		return nil, &PlannerError{Iteration: iteration, Reason: "planner handoff status=" + h.Status, Err: errors.New(h.Reason)}
	}
	if h.Kind != KindPlan {
		return nil, &PlannerError{Iteration: iteration, Reason: fmt.Sprintf("expected kind=plan, got %q", h.Kind)}
	}
	// Approval gate (spec § 0.4b step 2). When policy requires
	// approval for this iteration but the planner never emitted an
	// InquiryRequest during its turn, reject the handoff. The
	// inquired-flag is set by OnChildFrame on any
	// *protocol.InquiryRequest from the planner; absence after the
	// wave settled is conclusive.
	if approvalRequiredForIteration(manifest.Plan.Approval, iteration) {
		if !m.Inquired(h.Subagent.SessionID) {
			return nil, &PlannerError{
				Iteration: iteration,
				Reason:    "planner emitted plan without the required session:inquire approval",
			}
		}
	}
	plan, decodeErr := DecodePlan(h)
	if decodeErr != nil {
		return nil, &PlannerError{Iteration: iteration, Reason: "decode plan", Err: decodeErr}
	}
	return plan, nil
}

// spawnAndAwaitChecker runs one checker iteration. Spawns the
// manifest's control role under a `_check-<N>` wave, waits for the
// checker's kind=verdict handoff, decodes it. Returns the typed
// verdict.
//
// Mirrors spawnAndAwaitPlanner's contract — failure modes (wave
// error, handoff parse failure, decode failure, unknown decision)
// surface as *PlannerError so the caller's errors.As walk works
// across both phases.
func (e *Extension) spawnAndAwaitChecker(ctx context.Context, executor *Executor, mission extension.SessionState, manifest MissionManifest, missionSkill, _goal string, iteration int) (Verdict, error) {
	if manifest.Control.Role == "" {
		return Verdict{}, &PlannerError{Iteration: iteration, Reason: "manifest.control.role is empty"}
	}
	task, err := buildCheckerTask(mission, manifest, iteration)
	if err != nil {
		return Verdict{}, &PlannerError{Iteration: iteration, Reason: "build checker task", Err: err}
	}
	waveLabel := checkerWaveLabelPrefix + strconv.Itoa(iteration)
	wave := Wave{
		Label: waveLabel,
		Subagents: []SubagentSpec{{
			Name:  "checker",
			Skill: missionSkill,
			Role:  manifest.Control.Role,
			Task:  task,
		}},
	}
	status, _, runErr := executor.RunWave(ctx, mission, wave, RunWaveOptions{})
	e.emitWaveComplete(mission, waveLabel, status, runErr)
	if runErr != nil {
		return Verdict{}, &PlannerError{Iteration: iteration, Reason: "checker wave run", Err: runErr}
	}
	if status == WaveStatusFailed {
		return Verdict{}, &PlannerError{Iteration: iteration, Reason: "checker wave failed"}
	}
	ref, refErr := MakeRef("checker", waveLabel)
	if refErr != nil {
		return Verdict{}, &PlannerError{Iteration: iteration, Reason: "ref checker@" + waveLabel, Err: refErr}
	}
	m := FromState(mission)
	if m == nil {
		return Verdict{}, &PlannerError{Iteration: iteration, Reason: "no MissionState on session"}
	}
	h, ok := m.Handoffs.Get(ref)
	if !ok {
		return Verdict{}, &PlannerError{Iteration: iteration, Reason: "no checker handoff under " + ref}
	}
	if h.Status != "ok" {
		return Verdict{}, &PlannerError{Iteration: iteration, Reason: "checker handoff status=" + h.Status, Err: errors.New(h.Reason)}
	}
	if h.Kind != KindVerdict {
		return Verdict{}, &PlannerError{Iteration: iteration, Reason: fmt.Sprintf("expected kind=verdict, got %q", h.Kind)}
	}
	verdict, decodeErr := DecodeVerdict(h)
	if decodeErr != nil {
		return Verdict{}, &PlannerError{Iteration: iteration, Reason: "decode verdict", Err: decodeErr}
	}
	// When the checker emits decision=inquire, require that it
	// actually called session:inquire during its turn (parallel
	// to the planner approval gate from γ). The post-close flag
	// is set by OnChildFrame on any *protocol.InquiryRequest from
	// the checker.
	if verdict.Decision == VerdictInquire {
		if !m.Inquired(h.Subagent.SessionID) {
			return Verdict{}, &PlannerError{
				Iteration: iteration,
				Reason:    "checker emitted decision=inquire without the required session:inquire call",
			}
		}
	}
	return verdict, nil
}

// emitVerdictReady publishes a verdict_ready ExtensionFrame on the
// mission's event log so scenarios + liveview can observe checker
// decisions without parsing wave_complete frames. Carries enough
// shape to drive UI / harness assertions; full verdict body lives
// in the checker's handoff.
func (e *Extension) emitVerdictReady(mission extension.SessionState, iteration int, v Verdict) {
	payload := struct {
		Iteration int             `json:"iteration"`
		Decision  VerdictDecision `json:"decision"`
		Issues    []string        `json:"issues,omitempty"`
		Reason    string          `json:"reason,omitempty"`
	}{
		Iteration: iteration,
		Decision:  v.Decision,
		Issues:    v.Issues,
		Reason:    v.Reason,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		e.logger.Warn("mission: emitVerdictReady: marshal failed",
			"mission_session", mission.SessionID(), "iteration", iteration, "err", err)
		return
	}
	frame := protocol.NewExtensionFrame(
		mission.SessionID(),
		agentParticipant(mission, e.agentID),
		providerName,
		protocol.CategoryOp,
		"verdict_ready",
		data,
	)
	if err := mission.Emit(context.Background(), frame); err != nil {
		e.logger.Warn("mission: emitVerdictReady: emit failed",
			"mission_session", mission.SessionID(), "iteration", iteration, "err", err)
	}
}

// makeSpawnerCallback returns the Spawner closure executor uses to
// open child sessions. Mirrors driveMission's closure literal —
// extracted so both the inline and planner driver paths share one
// implementation (and tests can substitute it).
func (e *Extension) makeSpawnerCallback(mission extension.SessionState, spawner extension.SessionSpawner) Spawner {
	return func(ctx context.Context, _ extension.SessionState, req SpawnRequest) (SpawnResult, error) {
		child, err := spawner.SpawnChild(ctx, extension.SpawnSpec{
			Name:       req.Name,
			Skill:      req.Skill,
			Role:       req.Role,
			Task:       req.Task,
			Inputs:     req.Inputs,
			RenderMode: req.RenderMode,
		})
		if err != nil {
			return SpawnResult{}, err
		}
		first := protocol.NewUserMessage(child.SessionID(), agentParticipant(mission, e.agentID), req.Task)
		settled := child.Submit(context.Background(), first)
		return SpawnResult{SessionID: child.SessionID(), Settled: settled}, nil
	}
}

// plannerTaskView is the typed payload the planner_task template
// renders against. Kept narrow — Phase B passes only the goal,
// iteration index, and approval flag. Phase C adds RecentVerdict
// (issues + reason from the prior iteration's checker). Phase D
// extends with PlanContext.
type plannerTaskView struct {
	Goal             string
	Iteration        int
	MaxWaves         int
	ApprovalRequired bool
	// PlanContext is a placeholder for Phase D; rendered empty in
	// Phase C so the template's section header is stable.
	PlanContext []plannerContextEntry
	// Recent enumerates prior non-internal wave outcomes. Cheap
	// stand-in for the full Phase-D plan_context journal.
	Recent []plannerRecentEntry
	// RecentVerdict is the prior iteration's checker verdict
	// rendered into [Recent verdict]. Nil on iteration 1 and when
	// no control role is declared.
	RecentVerdict *plannerVerdictView
}

// plannerVerdictView is the per-iteration verdict projection the
// planner_task template renders under [Recent verdict].
type plannerVerdictView struct {
	Decision string
	Reason   string
	Issues   []string
}

// plannerContextEntry is the per-iteration context line the
// template renders under [Plan context]. Phase D fills these from
// the prior planner's memory_summary + the just-completed wave's
// status.
type plannerContextEntry struct {
	Iteration int
	Phase     string
	Summary   string
}

// plannerRecentEntry is the per-wave outcome the template renders
// under [Recent waves]. Phase D fills these from PlanState.Done.
type plannerRecentEntry struct {
	Wave   string
	Status string
}

// buildPlannerTask renders the planner subagent's first message
// via assets/prompts/mission/planner_task.tmpl. Approval-required
// flag is set when the iteration's policy demands a session:inquire
// before the handoff (Initial=required for iteration 1; Iteration
// policy for later spawns). Recent populated from PlanState.Done
// so the planner can tell what waves already ran (cheap stand-in
// for the full Phase-D plan_context journal). RecentVerdict, when
// non-nil, surfaces the prior iteration's checker decision +
// issues under [Recent verdict].
func buildPlannerTask(mission extension.SessionState, manifest MissionManifest, goal string, iteration int, recentVerdict *Verdict) (string, error) {
	approval := approvalRequiredForIteration(manifest.Plan.Approval, iteration)
	view := plannerTaskView{
		Goal:             goal,
		Iteration:        iteration,
		MaxWaves:         manifest.Plan.MaxWaves,
		ApprovalRequired: approval,
		Recent:           collectRecentWaves(mission),
		PlanContext:      collectPlanContext(mission),
	}
	if recentVerdict != nil {
		view.RecentVerdict = &plannerVerdictView{
			Decision: string(recentVerdict.Decision),
			Reason:   recentVerdict.Reason,
			Issues:   recentVerdict.Issues,
		}
	}
	renderer := mission.Prompts()
	if renderer == nil {
		return "", fmt.Errorf("mission: planner task: no prompts renderer on session")
	}
	return renderer.Render("mission/planner_task", view)
}

// collectPlanContext projects PlanContext.List() into the
// plannerContextEntry shape the planner / checker / synthesizer
// templates render. Phase D — every entry surfaces; FIFO trim
// inside PlanContext already bounded the size.
func collectPlanContext(mission extension.SessionState) []plannerContextEntry {
	m := FromState(mission)
	if m == nil || m.PlanContext == nil {
		return nil
	}
	rows := m.PlanContext.List()
	out := make([]plannerContextEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, plannerContextEntry{
			Iteration: r.Iteration,
			Phase:     r.Phase,
			Summary:   r.Summary,
		})
	}
	return out
}

// checkerTaskView is the typed payload the checker_task template
// renders against. Phase C — mission goal, iteration index, and
// the handoffs the checker should validate. Phase D — PlanContext
// projection so the checker sees prior-iteration memory summaries
// without re-reading every handoff body.
type checkerTaskView struct {
	Goal        string
	Iteration   int
	Handoffs    []synthesisHandoffView
	PlanContext []plannerContextEntry
}

// buildCheckerTask renders the checker subagent's first message
// via assets/prompts/mission/checker_task.tmpl. The template
// teaches the kind=verdict fence shape and includes every handoff
// produced by the just-completed wave for the checker to inspect,
// plus the plan_context journal for cross-iteration awareness.
func buildCheckerTask(mission extension.SessionState, _ MissionManifest, iteration int) (string, error) {
	view := checkerTaskView{
		Goal:        "",
		Iteration:   iteration,
		PlanContext: collectPlanContext(mission),
	}
	if m := FromState(mission); m != nil {
		for _, h := range m.Handoffs.List() {
			if strings.HasPrefix(h.Ref, "planner@") || strings.HasPrefix(h.Ref, "checker@") || strings.HasPrefix(h.Ref, "synthesizer@") {
				continue
			}
			view.Handoffs = append(view.Handoffs, synthesisHandoffView{
				Ref:           h.Ref,
				Role:          h.Subagent.Role,
				Skill:         h.Subagent.Skill,
				Status:        h.Status,
				MemorySummary: h.MemorySummary,
				Body:          synthesisHandoffBody(h.Body),
			})
		}
	}
	renderer := mission.Prompts()
	if renderer == nil {
		return "", fmt.Errorf("mission: checker task: no prompts renderer on session")
	}
	return renderer.Render("mission/checker_task", view)
}

// collectRecentWaves projects PlanState.Done into the template's
// [Recent waves] section. Skips planner / synthesis waves
// (underscore-prefixed labels) — those are runtime-internal and
// don't carry mission-meaningful work for the planner to react
// to. Empty when no waves have run yet (iteration 1).
func collectRecentWaves(mission extension.SessionState) []plannerRecentEntry {
	m := FromState(mission)
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]plannerRecentEntry, 0, len(m.Plan.Done))
	for _, w := range m.Plan.Done {
		if strings.HasPrefix(w.Label, "_") {
			continue
		}
		out = append(out, plannerRecentEntry{
			Wave:   w.Label,
			Status: string(w.Status),
		})
	}
	return out
}

// approvalRequiredForIteration applies the v1 approval policy to
// the given iteration index. Iteration 1 reads Initial; iterations
// >1 read Iteration. Empty values are treated as the spec defaults
// (initial=required, iteration=initial-only).
func approvalRequiredForIteration(policy PlanApproval, iteration int) bool {
	policy = NormalizePlanApproval(policy)
	if iteration <= 1 {
		return policy.Initial == ApprovalInitialRequired
	}
	switch policy.Iteration {
	case ApprovalIterationAlways:
		return true
	case ApprovalIterationNever, ApprovalIterationInitOnly:
		return false
	default:
		return false
	}
}

// decorateWaveTasks appends the bundled mission/worker_contract
// template to each subagent's task body. Workers spawned by the
// planner-driven loop see plain-prose instructions from the
// planner plus the canonical handoff-fence contract from the
// runtime — the planner doesn't have to remember to teach the
// fence shape itself.
//
// Returns a shallow copy of wave with decorated tasks; the
// original wave value is not mutated. Errors only when the prompts
// renderer can't load the contract template (a missing template
// is a runtime bug, not user input).
func decorateWaveTasks(mission extension.SessionState, wave Wave) (Wave, error) {
	renderer := mission.Prompts()
	if renderer == nil {
		return Wave{}, fmt.Errorf("mission: decorateWaveTasks: no prompts renderer on session")
	}
	contract, err := renderer.Render("mission/worker_contract", nil)
	if err != nil {
		return Wave{}, fmt.Errorf("mission: decorateWaveTasks: render worker_contract: %w", err)
	}
	out := wave
	out.Subagents = make([]SubagentSpec, len(wave.Subagents))
	copy(out.Subagents, wave.Subagents)
	for i := range out.Subagents {
		original := out.Subagents[i].Task
		if original == "" {
			out.Subagents[i].Task = contract
			continue
		}
		out.Subagents[i].Task = original + "\n\n" + contract
	}
	return out, nil
}

// missionHasHandoffs reports whether the mission's Handoffs store
// carries at least one entry — i.e. some wave produced a result.
// Used by the planner driver to skip a synthesis spawn that would
// have nothing to summarise (an empty plan_complete on iteration 1
// is the canonical case). Returns false when the mission has no
// MissionState attached.
func missionHasHandoffs(mission extension.SessionState) bool {
	m := FromState(mission)
	if m == nil {
		return false
	}
	return m.Handoffs.Len() > 0
}

// emitUserFollowup publishes a user_followup ExtensionFrame on
// the mission's event log. Phase E — fires from mission:notify
// after the followup lands in plan_context so adapters /
// scenarios can observe the delivery without polling the
// journal.
func (e *Extension) emitUserFollowup(mission extension.SessionState, text string) {
	payload := struct {
		Text string `json:"text"`
	}{Text: text}
	data, err := json.Marshal(payload)
	if err != nil {
		e.logger.Warn("mission: emitUserFollowup: marshal failed",
			"mission_session", mission.SessionID(), "err", err)
		return
	}
	frame := protocol.NewExtensionFrame(
		mission.SessionID(),
		agentParticipant(mission, e.agentID),
		providerName,
		protocol.CategoryOp,
		"user_followup",
		data,
	)
	if err := mission.Emit(context.Background(), frame); err != nil {
		e.logger.Warn("mission: emitUserFollowup: emit failed",
			"mission_session", mission.SessionID(), "err", err)
	}
}

// emitIterationStart publishes an iteration_start ExtensionFrame on
// the mission session's event log so liveview / scenario harnesses
// can observe planner spawns without parsing wave_complete frames.
// Phase B emits this synthetic event before each planner spawn;
// recovery (Phase B+) will use it as the resume anchor.
func (e *Extension) emitIterationStart(mission extension.SessionState, iteration, maxWaves int) {
	payload := struct {
		Iteration int `json:"iteration"`
		MaxWaves  int `json:"max_waves,omitempty"`
	}{Iteration: iteration, MaxWaves: maxWaves}
	data, err := json.Marshal(payload)
	if err != nil {
		e.logger.Warn("mission: emitIterationStart: marshal failed",
			"mission_session", mission.SessionID(), "iteration", iteration, "err", err)
		return
	}
	frame := protocol.NewExtensionFrame(
		mission.SessionID(),
		agentParticipant(mission, e.agentID),
		providerName,
		protocol.CategoryOp,
		"iteration_start",
		data,
	)
	if err := mission.Emit(context.Background(), frame); err != nil {
		e.logger.Warn("mission: emitIterationStart: emit failed",
			"mission_session", mission.SessionID(), "iteration", iteration, "err", err)
	}
}
