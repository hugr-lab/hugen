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
	executor := NewExecutor(e.makeSpawnerCallback(mission, spawner, missionSkill), e.logger)
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
			e.logger.Warn("mission: planner loop: planner closed with an invalid plan — handing back to planner for amend",
				"mission_session", mission.SessionID(),
				"iteration", iteration, "err", plannerErr)
			// A *PlannerError indicates the planner closed with a
			// malformed handoff (parse / decode / role / output-
			// contract failure). The handoff is NOT actionable, but
			// the loop is — surface the parser's reason to the next
			// planner spawn as a synthetic amend verdict so it can
			// re-emit a valid plan. max_waves caps the retry budget;
			// genuinely broken planners hit the cap and synthesis
			// recaps the partial state.
			recentVerdict = &Verdict{
				Decision: VerdictAmend,
				Reason:   "previous planner handoff was invalid — re-emit a clean plan",
				Issues:   []string{plannerErr.Error()},
			}
			continue
		}

		// 2. plan_complete signal — no more waves to run.
		if plan == nil {
			e.logger.Info("mission: planner loop: plan_complete received",
				"mission_session", mission.SessionID(),
				"iteration", iteration)
			return false, nil
		}

		// Phase I.10 — approval is the planner's in-turn job via
		// mission:validate_plan(request_approval=true). The gate
		// is enforced earlier in spawnAndAwaitPlanner — by the time
		// we reach this point with a non-nil plan, approval has
		// either succeeded or the iteration policy didn't require
		// it. No post-close fallback inquire.

		// 3. Run the planner-emitted wave. Each subagent's task is
		// decorated with the runtime-injected handoff contract so
		// the worker knows to end its turn with a fenced handoff
		// block — without this most weak models go off and start
		// executing the task literally (calling bash, hanging on
		// approval) instead of emitting a result fence.
		decorated, decorateErr := decorateWaveTasks(mission, manifest, plan.NextWave)
		if decorateErr != nil {
			e.logger.Warn("mission: planner loop: decorate wave tasks failed",
				"mission_session", mission.SessionID(), "iteration", iteration, "err", decorateErr)
			return true, decorateErr
		}
		status, _, runErr := executor.RunWave(ctx, mission, decorated, RunWaveOptions{})
		e.emitWaveComplete(mission, plan.NextWave.Label, status, runErr)
		if runErr != nil || status == WaveStatusFailed {
			e.logger.Warn("mission: planner loop: executed wave failed — handing to planner for amend",
				"mission_session", mission.SessionID(),
				"iteration", iteration,
				"wave", plan.NextWave.Label,
				"status", status,
				"err", runErr)
			// Fold wave failure into a synthetic verdict so the next
			// planner spawn sees the failure under [Recent verdict]
			// and replans rather than aborting the mission. Caps at
			// max_waves cleanly; consecutive identical failures get
			// flushed when the iteration counter hits the cap and
			// synthesis recaps whatever partial state was produced.
			recentVerdict = &Verdict{
				Decision: VerdictAmend,
				Reason:   fmt.Sprintf("wave %q failed", plan.NextWave.Label),
				Issues:   collectWaveFailureIssues(mission, plan.NextWave.Label, runErr),
			}
			continue
		}

		// 4. Phase C — verdict phase. Spawn checker if declared,
		// route on its decision. Absent control collapses to the
		// implicit `continue` path. Phase I.9: planner may set
		// `next_wave.skip_check: true` for trivial waves whose
		// verdict is obvious (one worker, status=ok); runtime
		// skips the checker spawn entirely. SkipCheck is ignored
		// on wave failures (those go through the synthetic amend
		// path above before reaching this block).
		recentVerdict = nil
		if plan.NextWave.SkipCheck {
			e.logger.Info("mission: planner loop: checker skipped per next_wave.skip_check",
				"mission_session", mission.SessionID(),
				"iteration", iteration, "wave", plan.NextWave.Label)
		} else if manifest.Control.Role != "" {
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
				// Phase I.26 — runtime gate. The checker may only
				// finish when every mission AC is satisfied. Any
				// unsatisfied entry in mission_ac_status coerces the
				// verdict into a synthetic amend with the gaps
				// folded into issues, so the next planner can revise
				// the plan to address them.
				if pendings := unsatisfiedMissionAC(verdict); len(pendings) > 0 {
					e.logger.Warn("mission: planner loop: rejecting finish — mission acceptance criteria unsatisfied",
						"mission_session", mission.SessionID(),
						"iteration", iteration,
						"unsatisfied", pendings)
					synthetic := Verdict{
						Decision: VerdictAmend,
						Reason:   "checker proposed finish but mission acceptance criteria are not all satisfied; replan to close the gaps",
						Issues:   pendings,
					}
					recentVerdict = &synthetic
					continue
				}
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
	// Decode the typed plan from the handoff. plan_complete
	// (next_wave=null) returns nil here; the caller treats that
	// as the mission's done-signal.
	plan, decodeErr := DecodePlan(h)
	if decodeErr != nil {
		return nil, &PlannerError{Iteration: iteration, Reason: "decode plan", Err: decodeErr}
	}

	// Phase I.23 — skill-agnostic approval gate. Mission state
	// carries ONE marker: the body the user most recently
	// approved (or empty when no plan is currently approved).
	// The planner MUST have called `mission:validate_and_approve`
	// with the same body it now emits in the handoff fence,
	// AND that body must produce the same canonical marker the
	// tool stamped. Mismatch → reject the handoff. Worker
	// invalidation (handoff body `invalidates_plan_approval:
	// true`) clears the marker between iterations, forcing the
	// next planner to re-validate from scratch.
	//
	// Missions whose policy opts out (Initial=skip) skip the
	// gate entirely — automation / test missions take the
	// approved-by-default branch.
	if approvalRequiredForIteration(manifest.Plan.Approval, iteration, m) {
		emittedMarker, markerErr := canonicalPlanMarker(h.Body)
		if markerErr != nil {
			return nil, &PlannerError{Iteration: iteration, Reason: "compute emitted plan marker", Err: markerErr}
		}
		stored := m.ApprovedPlanMarker()
		switch {
		case stored == "":
			return nil, &PlannerError{
				Iteration: iteration,
				Reason:    "planner emitted a plan handoff without a recorded approval; call mission:validate_and_approve with the body you intend to emit, then ship the fence verbatim",
			}
		case stored != emittedMarker:
			return nil, &PlannerError{
				Iteration: iteration,
				Reason: fmt.Sprintf(
					"plan body marker mismatch: approved=%s emitted=%s — the body you shipped in the handoff is not the body the user approved. Re-call mission:validate_and_approve with the body you intend to commit, or emit the previously-approved body verbatim.",
					stored, emittedMarker,
				),
			}
		}
	}
	// Phase I.26 — snapshot the planner's current mission frame
	// (goal restatement + mission AC + wave AC) so the checker sees
	// the latest definition of done. plan_complete (nil plan) skips
	// the snapshot — the existing frame remains as the synthesizer's
	// reference.
	if plan != nil && plan.NextWave.Label != "" {
		m.SetMissionFrame(plan.MissionGoal, plan.MissionAcceptanceCriteria, plan.NextWave.AcceptanceCriteria)
	}
	return plan, nil
}

// planIsResearchOnly was a phase-I.15 helper consulted by the
// approval gate; Phase I.23 dropped runtime knowledge of
// skill-specific role names in favour of worker-driven approval
// invalidation. Kept here as a stub to avoid breaking external
// callers — always returns false.
//
// Deprecated: do not call.
func planIsResearchOnly(plan *Plan) bool {
	_ = plan
	return false
}

// unsatisfiedMissionAC walks the checker's per-criterion mission
// AC report and returns a string slice of "<criterion> (evidence:
// <text>)" entries for every row whose `satisfied` flag is false.
// Empty MissionACStatus is treated as "every criterion still
// pending" — a checker proposing `finish` without filling AC
// status is misbehaving and the runtime keeps it honest by
// surfacing the gap via the synthetic-amend issue list. Phase I.26.
func unsatisfiedMissionAC(v Verdict) []string {
	if len(v.MissionACStatus) == 0 {
		return []string{
			"checker proposed finish but emitted no mission_ac_status — fill mission_ac_status[] with per-criterion satisfaction so the runtime can verify completion",
		}
	}
	var out []string
	for _, entry := range v.MissionACStatus {
		if entry.Satisfied {
			continue
		}
		msg := entry.Criterion
		if msg == "" {
			msg = "(unnamed criterion)"
		}
		if ev := strings.TrimSpace(entry.Evidence); ev != "" {
			msg = msg + " (evidence: " + ev + ")"
		} else {
			msg = msg + " (no evidence in handoffs)"
		}
		out = append(out, msg)
	}
	return out
}

// _unused keeps the deprecated planIsResearchOnly tied to its
// original sentinel-only signature for any test reference; the
// helper has no runtime callers as of Phase I.23.
func _unusedPlanIsResearchOnly(plan *Plan) bool {
	if plan == nil || plan.NextWave.Label == "" || len(plan.NextWave.Subagents) == 0 {
		return false
	}
	for _, s := range plan.NextWave.Subagents {
		if s.Role != "researcher" {
			return false
		}
	}
	return true
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
	task, err := buildCheckerTask(mission, manifest, _goal, iteration)
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
//
// missionSkill names the mission's dispatching skill (e.g.
// "analyst"); the closure substitutes it for any SpawnRequest with
// an empty Skill so role lookups (ApplyOnSubagentSpawn, intent
// override) resolve against the right manifest. The planner only
// emits role names from its own dispatching skill, so a missing
// `skill:` on a wave subagent always means "use the mission's".
func (e *Extension) makeSpawnerCallback(mission extension.SessionState, spawner extension.SessionSpawner, missionSkill string) Spawner {
	return func(ctx context.Context, _ extension.SessionState, req SpawnRequest) (SpawnResult, error) {
		effectiveSkill := req.Skill
		if effectiveSkill == "" {
			effectiveSkill = missionSkill
		}
		child, err := spawner.SpawnChild(ctx, extension.SpawnSpec{
			Name:       req.Name,
			Skill:      effectiveSkill,
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
	// DoRoles enumerates the manifest's `sub_agents` Do-roles
	// (non-planner / non-checker / non-synthesizer) so the planner
	// sees a closed catalogue and picks a real role name instead
	// of guessing `worker` and falling through to the generic
	// _worker autoload (which has no domain tools).
	DoRoles []plannerDoRoleView
}

// plannerDoRoleView is one row in the planner's
// [Available Do roles] catalogue.
type plannerDoRoleView struct {
	Role        string
	Description string
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
// Refs exposes the full handoff refs (`<name>@<wave>`) produced by
// the wave so the planner knows what addressable depends_on values
// are available for downstream subagents — without it the model
// tends to pass bare wave-labels which fail the ref lookup.
type plannerRecentEntry struct {
	Wave   string
	Status string
	Refs   []string
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
	approval := approvalRequiredForIteration(manifest.Plan.Approval, iteration, FromState(mission))
	doRoles := make([]plannerDoRoleView, 0, len(manifest.Workers))
	for _, w := range manifest.Workers {
		if w.Role == "" {
			continue
		}
		doRoles = append(doRoles, plannerDoRoleView{
			Role:        w.Role,
			Description: w.Description,
		})
	}
	view := plannerTaskView{
		Goal:             goal,
		Iteration:        iteration,
		MaxWaves:         manifest.Plan.MaxWaves,
		ApprovalRequired: approval,
		DoRoles:          doRoles,
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
// without re-reading every handoff body. Phase I.25 — Goal +
// PendingRoadmap surfaced so the checker can refuse `finish`
// when the user goal still has unexecuted roadmap commitments.
type checkerTaskView struct {
	Goal              string
	PlannerGoal       string
	MissionAC         []string
	WaveAC            []string
	Iteration         int
	Handoffs          []synthesisHandoffView
	PlanContext       []plannerContextEntry
	PendingRoadmap    []plannerRoadmapView
}

// plannerRoadmapView projects one un-executed roadmap entry from
// the most recent planner handoff, used by the checker's
// completeness gate.
type plannerRoadmapView struct {
	Label       string
	Description string
}

// buildCheckerTask renders the checker subagent's first message
// via assets/prompts/mission/checker_task.tmpl. The template
// teaches the kind=verdict fence shape and includes every handoff
// produced by the just-completed wave for the checker to inspect,
// plus the plan_context journal for cross-iteration awareness,
// the user goal, and any roadmap entries the planner committed to
// but the runtime hasn't executed yet.
func buildCheckerTask(mission extension.SessionState, _ MissionManifest, goal string, iteration int) (string, error) {
	view := checkerTaskView{
		Goal:           goal,
		Iteration:      iteration,
		PlanContext:    collectPlanContext(mission),
		PendingRoadmap: collectPendingRoadmap(mission),
	}
	if m := FromState(mission); m != nil {
		view.PlannerGoal, view.MissionAC, view.WaveAC = m.MissionFrame()
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

// collectPendingRoadmap walks the most recent planner handoff's
// roadmap and filters out entries whose `label` already appears
// as a Done wave label in PlanState — what's left is "the planner
// promised this and the mission hasn't done it yet". The checker
// sees this list and refuses `finish` when it isn't empty.
func collectPendingRoadmap(mission extension.SessionState) []plannerRoadmapView {
	m := FromState(mission)
	if m == nil {
		return nil
	}
	// Find the most recent planner handoff and its roadmap.
	var latestPlan *Plan
	for _, h := range m.Handoffs.List() {
		if !strings.HasPrefix(h.Ref, "planner@") {
			continue
		}
		p, err := DecodePlan(h)
		if err != nil {
			continue
		}
		latestPlan = p
	}
	if latestPlan == nil || len(latestPlan.Roadmap) == 0 {
		return nil
	}
	done := map[string]bool{}
	for _, dw := range m.Plan.Done {
		if dw.Label != "" {
			done[dw.Label] = true
		}
	}
	if latestPlan.NextWave.Label != "" {
		done[latestPlan.NextWave.Label] = true
	}
	out := make([]plannerRoadmapView, 0, len(latestPlan.Roadmap))
	for _, r := range latestPlan.Roadmap {
		if r.Label == "" || done[r.Label] {
			continue
		}
		out = append(out, plannerRoadmapView{Label: r.Label, Description: r.Description})
	}
	return out
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
		refs := make([]string, 0, len(w.Subagents))
		for _, s := range w.Subagents {
			if s.Ref != "" {
				refs = append(refs, s.Ref)
			}
		}
		out = append(out, plannerRecentEntry{
			Wave:   w.Label,
			Status: string(w.Status),
			Refs:   refs,
		})
	}
	return out
}

// collectWaveFailureIssues drains per-subagent error reasons from
// the just-failed wave's PlanState entry plus the wave-level error
// (when one was reported) into the issue list the planner will see
// under [Recent verdict] on its next spawn. Empty when the wave
// failed without recording per-subagent diagnostics — the
// synthetic verdict's `reason` still carries the wave label.
func collectWaveFailureIssues(mission extension.SessionState, waveLabel string, runErr error) []string {
	var issues []string
	if m := FromState(mission); m != nil {
		m.mu.Lock()
		for i := len(m.Plan.Done) - 1; i >= 0; i-- {
			w := m.Plan.Done[i]
			if w.Label != waveLabel {
				continue
			}
			for _, s := range w.Subagents {
				if s.Status == "error" && s.Error != "" {
					issues = append(issues, fmt.Sprintf("%s (role %s): %s", s.Name, s.Role, s.Error))
				}
			}
			break
		}
		m.mu.Unlock()
	}
	if runErr != nil {
		issues = append(issues, "wave run error: "+runErr.Error())
	}
	return issues
}

// approvalRequiredForIteration reports whether the mission's
// approval policy demands a user approval for iteration's plan.
//
// Phase I.23 simplification — approval is policy-driven only, no
// per-iteration scaling. Every planner iteration that emits a
// non-empty next_wave runs the inquire; the only opt-out is
// `policy.Initial == ApprovalInitialSkip`, which disables the
// approval gate for the entire mission. The `iteration` arg is
// kept on the signature for telemetry and future per-iteration
// policy work, but is unused under the current rule.
//
// Rationale: the original tri-state (initial-only / always /
// never) added confusion without operational value. Weak models
// tripped on "this iter needs approval, that one doesn't" cues,
// and the research-first pattern made the "initial iteration"
// itself ambiguous. A uniform "every plan emits an inquire"
// matches the user mental model and lets the marker discipline
// in spawnAndAwaitPlanner stay symmetric.
func approvalRequiredForIteration(policy PlanApproval, _ int, _ *MissionState) bool {
	policy = NormalizePlanApproval(policy)
	return policy.Initial != ApprovalInitialSkip
}

// workerContractView is the typed payload the
// mission/worker_contract template renders against. PlanContext
// is rendered only for roles that opt-in via the manifest's
// `capabilities.plan_context: read` knob; for everyone else the
// section is omitted entirely (template gates on the slice being
// non-empty). Phase F.
type workerContractView struct {
	PlanContext []plannerContextEntry
}

// decorateWaveTasks appends the bundled mission/worker_contract
// template to each subagent's task body. Workers spawned by the
// planner-driven loop see plain-prose instructions from the
// planner plus the canonical handoff-fence contract from the
// runtime — the planner doesn't have to remember to teach the
// fence shape itself.
//
// Phase F — workers whose role declares `capabilities.plan_context:
// read` (or whose role-class default resolves to read) get a
// [Plan context] section prepended to the contract. Resolution
// happens per subagent so a mixed wave (Do role + reading role)
// emits the right contract for each.
//
// Returns a shallow copy of wave with decorated tasks; the
// original wave value is not mutated. Errors only when the prompts
// renderer can't load the contract template (a missing template
// is a runtime bug, not user input).
func decorateWaveTasks(mission extension.SessionState, manifest MissionManifest, wave Wave) (Wave, error) {
	renderer := mission.Prompts()
	if renderer == nil {
		return Wave{}, fmt.Errorf("mission: decorateWaveTasks: no prompts renderer on session")
	}
	planCtx := collectPlanContext(mission)
	out := wave
	out.Subagents = make([]SubagentSpec, len(wave.Subagents))
	copy(out.Subagents, wave.Subagents)
	for i := range out.Subagents {
		view := workerContractView{}
		if ResolvePlanContextAccess(manifest, out.Subagents[i].Role) == PlanContextRead {
			view.PlanContext = planCtx
		}
		contract, err := renderer.Render("mission/worker_contract", view)
		if err != nil {
			return Wave{}, fmt.Errorf("mission: decorateWaveTasks: render worker_contract: %w", err)
		}
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
