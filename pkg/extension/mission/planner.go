package mission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

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
	if !aborted && manifest.Synthesis.Role != "" {
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

	for iteration := 1; iteration <= maxIter; iteration++ {
		e.emitIterationStart(mission, iteration, manifest.Plan.MaxWaves)

		// 1. Spawn the planner subagent.
		plan, plannerErr := e.spawnAndAwaitPlanner(ctx, executor, mission, manifest, missionSkill, goal, iteration)
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

		// 3. Run the planner-emitted wave.
		status, _, runErr := executor.RunWave(ctx, mission, plan.NextWave, RunWaveOptions{})
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
func (e *Extension) spawnAndAwaitPlanner(ctx context.Context, executor *Executor, mission extension.SessionState, manifest MissionManifest, missionSkill, goal string, iteration int) (*Plan, error) {
	if manifest.Plan.Role == "" {
		return nil, &PlannerError{Iteration: iteration, Reason: "manifest.plan.role is empty"}
	}
	task, err := buildPlannerTask(mission, manifest, goal, iteration)
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
// iteration index, and approval flag. Phase D extends with
// PlanContext + Recent.
type plannerTaskView struct {
	Goal             string
	Iteration        int
	MaxWaves         int
	ApprovalRequired bool
	// PlanContext / Recent are placeholders for Phase D; rendered
	// empty in Phase B so the template's section headers are
	// stable.
	PlanContext []plannerContextEntry
	Recent      []plannerRecentEntry
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
// policy for later spawns).
func buildPlannerTask(mission extension.SessionState, manifest MissionManifest, goal string, iteration int) (string, error) {
	approval := approvalRequiredForIteration(manifest.Plan.Approval, iteration)
	view := plannerTaskView{
		Goal:             goal,
		Iteration:        iteration,
		MaxWaves:         manifest.Plan.MaxWaves,
		ApprovalRequired: approval,
	}
	renderer := mission.Prompts()
	if renderer == nil {
		return "", fmt.Errorf("mission: planner task: no prompts renderer on session")
	}
	return renderer.Render("mission/planner_task", view)
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
