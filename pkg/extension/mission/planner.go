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

// maxConsecutiveErrors caps how many wave failures or planner
// parse failures the runtime tolerates in a row before bailing
// out of the planner loop and routing to synthesis. Distinct from
// max_waves — that's the overall iteration budget, this is a
// tighter "retry budget" so a single broken wave can't burn the
// whole budget on amend cycles. Resets on the first successful
// wave. 3 is the dogfood-validated value: enough for a weak model
// to recover (planner-amend-on-invalid-handoff usually clears on
// retry-1 or retry-2) without letting a genuinely-stuck wave
// monopolise the rest of the budget.
const maxConsecutiveErrors = 5

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
	executor := NewExecutor(e.makeSpawnerCallback(mission, spawner, missionSkill), e.logger).
		WithTerminator(e.makeTerminatorCallback(spawner)).
		WithWaveHook(e.snapshotSpec)
	ctx := context.Background()
	_ = inputs

	// Phase 5.x — B15. Optional pre-planner research stage.
	// Surfaces user clarifications + scope findings into mission
	// state before the planner spawn so iter-1 plans see
	// resolved inputs (not "is it markdown or html?" left for the
	// planner to ask later via a researcher wave).
	researchAborted, researchErr := e.runResearchStage(ctx, executor, mission, manifest, missionSkill, goal)
	if researchErr != nil {
		e.logger.Warn("mission: driveMissionPlanner: research stage failed",
			"mission_session", mission.SessionID(),
			"err", researchErr, "aborted", researchAborted)
	}

	// A research abort (infeasible / no usable handoff / researcher
	// budget) ends the mission — there's no plan to run. Otherwise run
	// the planner loop. Either way the mission flows to synthesis below:
	// on an abort the synthesizer reports what happened + summarises
	// partial findings (the "normal path"), so the parent gets a
	// coherent message rather than a terse failure recap it might
	// silently re-attempt. Phase 5.2.
	aborted := researchAborted
	if !aborted {
		var err error
		aborted, err = e.runPlannerLoop(ctx, executor, mission, manifest, missionSkill, goal)
		if err != nil {
			e.logger.Warn("mission: driveMissionPlanner: planner loop failed",
				"mission_session", mission.SessionID(), "err", err)
		}
	}

	synthText := e.maybeSynthesize(ctx, executor, mission, manifest, missionSkill, goal, aborted)
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

	// consecutiveErrors tracks back-to-back planner-parse failures
	// and wave-execution failures. Reset on any successful wave.
	// Caps at [maxConsecutiveErrors] — a single stuck wave can't
	// monopolise the entire max_waves budget on amend cycles.
	consecutiveErrors := 0

	for iteration := 1; iteration <= maxIter; iteration++ {
		// Stamp the iteration on MissionState so ingestHandoff
		// tags plan_context entries with the right number even
		// when the handoff arrives between iterations.
		if m := FromState(mission); m != nil {
			// Phase 5.2 — a planner / checker from a PRIOR iteration
			// crossed its context budget. Abort cleanly now instead of
			// re-spawning into the same shortfall (or burning the
			// consecutive-error retry budget).
			if _, ok := m.BudgetAbortInfo(); ok {
				e.logger.Warn("mission: planner loop: aborting — orchestration role exceeded context budget",
					"mission_session", mission.SessionID(), "iteration", iteration)
				return true, nil
			}
			m.mu.Lock()
			m.IterationCounter = iteration
			m.mu.Unlock()
		}
		e.emitIterationStart(mission, iteration, manifest.Plan.MaxWaves)

		// 1. Spawn the planner subagent.
		plan, plannerErr := e.spawnAndAwaitPlanner(ctx, executor, mission, manifest, missionSkill, goal, iteration, recentVerdict)
		if plannerErr != nil {
			// Phase 6.x — user cancelled the plan at the approval modal.
			// End the mission as a cancellation (MissionState.cancelled
			// already stamped); synthesis is skipped via aborted=true and
			// buildFinalText renders the cancellation recap.
			if errors.Is(plannerErr, errPlannerAborted) {
				e.logger.Info("mission: planner loop: user cancelled the plan at approval",
					"mission_session", mission.SessionID(), "iteration", iteration)
				return true, nil
			}
			consecutiveErrors++
			e.logger.Warn("mission: planner loop: planner closed with an invalid plan — handing back to planner for amend",
				"mission_session", mission.SessionID(),
				"iteration", iteration,
				"consecutive_errors", consecutiveErrors,
				"err", plannerErr)
			if consecutiveErrors >= maxConsecutiveErrors {
				e.logger.Warn("mission: planner loop: aborting — consecutive error cap reached",
					"mission_session", mission.SessionID(),
					"iteration", iteration,
					"consecutive_errors", consecutiveErrors,
					"cap", maxConsecutiveErrors)
				return true, nil
			}
			// A *PlannerError indicates the planner closed with a
			// malformed handoff (parse / decode / role / output-
			// contract failure). The handoff is NOT actionable, but
			// the loop is — surface the parser's reason to the next
			// planner spawn as a synthetic amend verdict so it can
			// re-emit a valid plan. max_waves caps the retry budget;
			// consecutiveErrors caps the retry budget per stuck
			// wave so an unrecoverable error doesn't burn the full
			// iteration cap on amend cycles.
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
		status, _, runErr := executor.RunWave(ctx, mission, decorated,
			RunWaveOptions{RoleTimeout: manifest.TimeoutForRole})
		e.emitWaveComplete(mission, plan.NextWave.Label, status, runErr)
		if runErr != nil || status == WaveStatusFailed {
			consecutiveErrors++
			e.logger.Warn("mission: planner loop: executed wave failed — handing to planner for amend",
				"mission_session", mission.SessionID(),
				"iteration", iteration,
				"wave", plan.NextWave.Label,
				"status", status,
				"consecutive_errors", consecutiveErrors,
				"err", runErr)
			if consecutiveErrors >= maxConsecutiveErrors {
				e.logger.Warn("mission: planner loop: aborting — consecutive error cap reached",
					"mission_session", mission.SessionID(),
					"iteration", iteration,
					"consecutive_errors", consecutiveErrors,
					"cap", maxConsecutiveErrors)
				return true, nil
			}
			// Fold wave failure into a synthetic verdict so the next
			// planner spawn sees the failure under [Recent verdict]
			// and replans rather than aborting the mission. Caps at
			// max_waves cleanly; consecutiveErrors caps the retry
			// budget per stuck wave so an unrecoverable failure
			// doesn't burn the full iteration cap on amend cycles.
			recentVerdict = &Verdict{
				Decision: VerdictAmend,
				Reason:   fmt.Sprintf("wave %q failed", plan.NextWave.Label),
				Issues:   collectWaveFailureIssues(mission, plan.NextWave.Label, runErr),
			}
			continue
		}
		// Wave succeeded — do NOT reset consecutiveErrors yet.
		// The checker still runs below; resetting here would mask a
		// chronically-broken checker (every wave succeeds, every
		// checker fails — counter would bounce back to 0 every iter
		// and the cap would never trip). Reset happens at the END
		// of the iteration after BOTH wave + checker succeed.

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
				consecutiveErrors++
				e.logger.Warn("mission: planner loop: checker failed — folding into amend verdict for next iter",
					"mission_session", mission.SessionID(),
					"iteration", iteration,
					"consecutive_errors", consecutiveErrors,
					"err", checkErr)
				if consecutiveErrors >= maxConsecutiveErrors {
					e.logger.Warn("mission: planner loop: aborting — consecutive error cap reached on checker fail",
						"mission_session", mission.SessionID(),
						"iteration", iteration,
						"consecutive_errors", consecutiveErrors,
						"cap", maxConsecutiveErrors)
					return true, checkErr
				}
				// Synthetic amend so the next planner sees the
				// checker failure under [Recent verdict] and can
				// keep the mission moving instead of dying on a
				// transient parse glitch in the checker role.
				recentVerdict = &Verdict{
					Decision: VerdictAmend,
					Reason:   "previous checker handoff failed to parse — re-emit a clean wave; the prior wave's outputs are intact",
					Issues:   []string{checkErr.Error()},
				}
				continue
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
				// Phase 5.x B11 §3.7 — runtime gate. The checker
				// may only finish when every mission AC row is
				// satisfied (status=satisfied OR status=dropped).
				// Any still-unsatisfied row coerces the verdict
				// into a synthetic amend with the gaps folded into
				// issues, so the next planner can revise the plan
				// to address them. AC state comes from MissionState
				// (which has folded the checker's ac_update[] +
				// any worker satisfies channel on this iter
				// already), not from the verdict payload itself.
				if pendings := unsatisfiedMissionAC(FromState(mission)); len(pendings) > 0 {
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
		// End-of-iteration success path — wave AND checker (when
		// declared) both succeeded. Reset the consecutive-error
		// budget so the next stuck wave / checker gets a fresh
		// retry budget independent of prior failures earlier in
		// the mission.
		consecutiveErrors = 0
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
	m := FromState(mission)
	if m == nil {
		return nil, &PlannerError{Iteration: iteration, Reason: "no MissionState on session"}
	}
	task, err := buildPlannerTask(mission, manifest, goal, iteration, recentVerdict)
	if err != nil {
		return nil, &PlannerError{Iteration: iteration, Reason: "build planner task", Err: err}
	}
	// Phase 6.x — clear any prior iteration's staged validate_and_approve
	// outcome so the TurnFinalizeGate and the plan-read below see only
	// THIS planner's submission, never a stale approve.
	m.ResetPlannerSubmission()
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
	status, _, runErr := executor.RunWave(ctx, mission, wave,
		RunWaveOptions{RoleTimeout: manifest.TimeoutForRole})
	e.emitWaveComplete(mission, waveLabel, status, runErr)
	if runErr != nil {
		return nil, &PlannerError{Iteration: iteration, Reason: "planner wave run", Err: runErr}
	}

	// Phase 6.x — the plan is submitted via the single channel
	// mission:validate_and_approve and staged on MissionState; read it
	// here instead of parsing a terminal ```plan``` fence. The
	// TurnFinalizeGate held the planner's turn open until its
	// validate_and_approve reached a terminal verdict (approve / abort)
	// or the gate's retry cap was hit. ingestPlannerCompletion records
	// the wave's OK/error handoff from that staged verdict, so a failed
	// wave status here means the planner closed without an approved or
	// aborted plan.
	sub := m.PlannerSubmission()
	if sub.aborted {
		// User declined the plan at the approval modal — terminate the
		// mission as a cancellation, not a generic failure.
		m.MarkCancelled(sub.reason)
		return nil, errPlannerAborted
	}
	if status == WaveStatusFailed || !sub.approved {
		reason := "planner closed without submitting an approved plan via mission:validate_and_approve"
		switch {
		case sub.called && !sub.valid && len(sub.errs) > 0:
			reason = "planner closed with an invalid plan: " + strings.Join(sub.errs, "; ")
		case !sub.called:
			reason = "planner never called mission:validate_and_approve"
		}
		return nil, &PlannerError{Iteration: iteration, Reason: reason}
	}

	// Approved. plan_complete (nil plan, next_wave=null) is the
	// mission's done-signal — the caller treats a nil plan as such.
	plan := sub.plan
	// Phase 5.x — B11 §3.2: every AC mutation already funnelled through
	// callValidateAndApprove (stage / commit / apply-status-only) at
	// modal time, so the AC state is reconciled. We only stamp the goal
	// restatement + per-wave AC slot — both non-load-bearing across
	// approval gates (the contract part of `[Mission acceptance
	// criteria]` is the structured state.AC validate_and_approve
	// committed).
	if plan != nil && plan.NextWave.Label != "" {
		m.SetGoalAndWaveAC(plan.MissionGoal, plan.NextWave.AcceptanceCriteria)
	}
	return plan, nil
}

// errPlannerAborted is the sentinel spawnAndAwaitPlanner returns when
// the user aborted the plan at the approval modal. runPlannerLoop
// detects it (errors.Is) and ends the mission as a cancellation
// rather than routing through the generic wave-failure abort. Phase
// 6.x.
var errPlannerAborted = errors.New("mission: planner aborted by user at approval")

// unsatisfiedMissionAC reads state.AC and returns one
// "<id>: <statement>" entry per row whose status is still
// unsatisfied (dropped rows are excluded — they're out of contract).
//
// Empty state.AC after iter 1 is misbehaving — the planner should
// have seeded at least one AC. The caller's finish-gate emits the
// synthetic amend issues from this slice; an empty list means the
// mission is genuinely done.
//
// Phase 5.x — B11 §3.7 (was Phase I.26 reading from verdict).
func unsatisfiedMissionAC(m *MissionState) []string {
	if m == nil {
		return nil
	}
	rows := m.UnsatisfiedAC()
	if len(rows) == 0 {
		return nil
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		msg := row.ID + ": " + row.Statement
		if ev := strings.TrimSpace(row.LastEvidence); ev != "" {
			msg = msg + " (last evidence: " + ev + ")"
		} else {
			msg = msg + " (no evidence yet)"
		}
		out = append(out, msg)
	}
	return out
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
	status, _, runErr := executor.RunWave(ctx, mission, wave,
		RunWaveOptions{RoleTimeout: manifest.TimeoutForRole})
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
	// Phase 5.x B11 §3.5 — fold the checker's per-id ac_update[]
	// into state.AC before the finish gate reads it. Validation
	// already enforced status-only (no statement / drop) at the
	// output_contract layer, so the runtime call is safe to apply.
	if len(verdict.ACUpdate) > 0 {
		evidenceSource := "checker iter-" + strconv.Itoa(iteration)
		if err := m.ApplyStatusOnly(verdict.ACUpdate, iteration, evidenceSource); err != nil {
			return Verdict{}, &PlannerError{Iteration: iteration, Reason: "apply checker ac_update", Err: err}
		}
		// Re-project spec.md so on-disk AC statuses reflect the
		// checker's verification — the wave hook fired earlier, before
		// the verdict was decoded and applied. Phase B39.
		e.writeSpecContract(mission, m, nil)
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
		// Mission ext owns the worker's first-message contract — render
		// the [Inputs from parent] block here so the planner's lifted
		// `inputs.<key>` actually reach the worker LLM. Until this fix
		// the values flowed into SpawnSpec.Inputs (parent-side
		// SubagentStarted frame metadata) but never into the worker's
		// own context, leaving workers with task references like
		// "save to inputs.file_path" but no concrete value to deref.
		body := buildWorkerFirstMessage(req.Task, req.Inputs)
		first := protocol.NewUserMessage(child.SessionID(), agentParticipant(mission, e.agentID), body)
		settled := child.Submit(context.Background(), first)
		return SpawnResult{SessionID: child.SessionID(), Settled: settled}, nil
	}
}

// makeTerminatorCallback returns the [Terminator] the executor uses to
// cancel a worker that overran its per-role time budget. It delegates
// to the parent (mission) session's CancelChild — the workers are this
// session's children, so cancelling them stops the detached worker
// turn instead of letting it run on orphaned. The bool from CancelChild
// (false = no live child) is not load-bearing for the executor, which
// only needs "did the cancel error".
func (e *Extension) makeTerminatorCallback(spawner extension.SessionSpawner) Terminator {
	return func(ctx context.Context, sessionID, reason string) error {
		_, err := spawner.CancelChild(ctx, sessionID, reason)
		return err
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
	// RoleProse is the planner role's behavioral brief
	// (`sub_agents[].prompt`), rendered into the template's
	// `[Your role]` slot. Empty → the bare universal template.
	// Phase B34.
	RoleProse string
	// FirstPlanGate signals "no plan has been approved yet on this
	// mission". Drives the long-form [STOP — pre-flight checklist]
	// rendering on the planner's first gate-bearing turn so the
	// model learns the validate_and_approve discipline; subsequent
	// iterations see a short one-line reminder instead, saving
	// ~1.5K chars of prompt context. Sourced from
	// MissionState.IsPlanApproved() — false = first gate-bearing
	// iteration. Phase 5.x — B15 follow-up.
	FirstPlanGate bool
	// PendingReapproval signals that a worker handoff invalidated
	// the prior plan approval since the last modal closed. Renders
	// the [pending_reapproval] section so the planner restates the
	// goal/AC honestly given the new findings, then calls
	// validate_and_approve to re-open the modal. Phase 5.x — B13.
	PendingReapproval       bool
	PendingReapprovalReason string
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
	// ResearchFindings is the research role's done=true narrative.
	// Rendered under [Plan context] so the planner sees what the
	// research stage discovered before drafting iter-1. Empty when
	// the skill declares no mission.research block or when the
	// stage was skipped by trigger predicate. Phase 5.x — B15.
	ResearchFindings string
	// ResolvedUserInputs lists the user-confirmed key/value pairs
	// the research stage pulled out via clarifications. Planner
	// reads it to lift `file_path` / `output_format` / etc. into
	// workers' `inputs` without re-asking the user. Phase 5.x — B15.
	ResolvedUserInputs []researchKV
	// SpawnInputs lists the structured key/value pairs the caller
	// (root / parent mission) passed to `session:spawn_mission`
	// at spawn time. Distinct from `ResolvedUserInputs` (which the
	// research stage populates via clarifications) — these are
	// authoritative caller intent, NOT inferred from user prose.
	// The planner MUST propagate them verbatim to relevant
	// worker subagents via their `inputs` field. Empty when the
	// caller spawned without inputs. Phase 5.x-followup.
	SpawnInputs []researchKV
	// ResearchACProposals lists the proposed acceptance criteria
	// research recommends for planner consideration. Planner is
	// the authority — proposals are input only (§3.2.1). Phase
	// 5.x — B15.
	ResearchACProposals []researchACProposalView
	// Roadmap is the roadmap the planner committed to in its most
	// recent plan — every entry, each flagged Done when a wave with
	// that label already ran. Surfaced in [Roadmap] so the planner
	// sees its own plan-ahead (what it intended next) and doesn't lose
	// the thread across iterations or re-derive a fresh plan each
	// turn. Empty before the first plan or when no roadmap was set.
	Roadmap []plannerRoadmapView
	// MissionAC is the current acceptance-criteria roster (with
	// identity) rendered into [Mission acceptance criteria]. Rows
	// stay visible across the lifecycle — dropped entries surface
	// so the planner knows not to re-propose them; satisfied rows
	// remain so the planner sees what's already covered. Empty on
	// iter 1 before any manifest / research seeding. Phase 5.x —
	// B11 §3.4.
	MissionAC []plannerACView
}

// plannerACView is one row of the [Mission acceptance criteria]
// table the planner reads at every iteration. Carries enough
// context for the planner to:
//   - reference rows by `id` in `ac_update`,
//   - skip status-only re-emits (status already tracked),
//   - avoid re-proposing dropped rows by accident.
type plannerACView struct {
	ID            string
	Statement     string
	Origin        string
	Status        string
	LastEvidence  string
	AddedAtIter   int
	SatisfiedIter int
	Dropped       bool
	DropReason    string
}

// researchACProposalView is one row in the [Research AC proposals]
// section of the planner's first message.
type researchACProposalView struct {
	Statement string
	Rationale string
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
		RoleProse:        renderRoleProse(mission, manifest.RolePrompts[manifest.Plan.Role]),
	}
	if m := FromState(mission); m != nil {
		// FirstPlanGate: planner has never gotten an approve yet.
		// Drives the long-form STOP-checklist on the very first
		// gate-bearing iteration so weak models learn the
		// validate_and_approve discipline; subsequent iterations
		// get a short one-line reminder. Phase 5.x — B15 follow-up.
		view.FirstPlanGate = !m.IsPlanApproved()
		if pending, reason := m.PendingReapproval(); pending {
			view.PendingReapproval = true
			view.PendingReapprovalReason = reason
		}
		findings, resolvedInputs, acProposals := m.ResearchOutput()
		view.ResearchFindings = findings
		view.ResolvedUserInputs = projectResolvedInputsForTemplate(resolvedInputs)
		view.ResearchACProposals = projectACProposalsForTemplate(acProposals)
		view.MissionAC = projectMissionACForTemplate(m.ACSnapshot())
		// Phase 5.x-followup — surface the caller's spawn-time
		// inputs so the planner propagates them verbatim to
		// worker `inputs` (file_path, output_format, etc.) and
		// doesn't invent values from goal prose.
		view.SpawnInputs = projectResolvedInputsForTemplate(m.SpawnInputs())
	}
	view.Roadmap = collectRoadmap(mission)
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

// projectMissionACForTemplate copies state.AC rows into the
// flat plannerACView shape the planner_task template renders.
// Dropped rows still surface so the planner sees not to
// re-propose them (with the "dropped" flag + reason); satisfied
// rows surface so the planner can see what's already covered
// without re-emitting status. Phase 5.x — B11 §3.4.
func projectMissionACForTemplate(rows []AcceptanceCriterion) []plannerACView {
	if len(rows) == 0 {
		return nil
	}
	out := make([]plannerACView, 0, len(rows))
	for _, r := range rows {
		out = append(out, plannerACView{
			ID:            r.ID,
			Statement:     r.Statement,
			Origin:        r.Origin,
			Status:        string(r.Status),
			LastEvidence:  r.LastEvidence,
			AddedAtIter:   r.AddedAtIter,
			SatisfiedIter: r.SatisfiedAtIter,
			Dropped:       r.Status == ACDropped,
			DropReason:    r.DropReason,
		})
	}
	return out
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
	Goal           string
	PlannerGoal    string
	MissionAC      []plannerACView
	WaveAC         []string
	Iteration      int
	Handoffs       []synthesisHandoffView
	PlanContext    []plannerContextEntry
	PendingRoadmap []plannerRoadmapView
	// RoleProse is the control (checker) role's behavioral brief
	// (`sub_agents[].prompt`), rendered into the template's
	// `[Your role]` slot. Empty → the bare universal template.
	// Phase B34.
	RoleProse string
}

// plannerRoadmapView projects one roadmap entry from the most recent
// planner handoff. Used by the checker's completeness gate (pending
// entries only) and by the planner's own [Roadmap] section (all
// entries, with Done marking which labels already ran).
type plannerRoadmapView struct {
	Label       string
	Description string
	Done        bool
}

// buildCheckerTask renders the checker subagent's first message
// via assets/prompts/mission/checker_task.tmpl. The template
// teaches the kind=verdict fence shape and includes every handoff
// produced by the just-completed wave for the checker to inspect,
// plus the plan_context journal for cross-iteration awareness,
// the user goal, and any roadmap entries the planner committed to
// but the runtime hasn't executed yet.
func buildCheckerTask(mission extension.SessionState, manifest MissionManifest, goal string, iteration int) (string, error) {
	view := checkerTaskView{
		Goal:           goal,
		Iteration:      iteration,
		PlanContext:    collectPlanContext(mission),
		PendingRoadmap: collectPendingRoadmap(mission),
		RoleProse:      renderRoleProse(mission, manifest.RolePrompts[manifest.Control.Role]),
	}
	if m := FromState(mission); m != nil {
		// PlannerGoal + WaveAC pull from MissionFrame (which projects
		// state.AC for legacy callers); for the structured roster we
		// use ACSnapshot() directly so the checker sees stable ids.
		goalProjection, _, waveProjection := m.MissionFrame()
		view.PlannerGoal = goalProjection
		view.WaveAC = waveProjection
		view.MissionAC = projectMissionACForTemplate(m.ACSnapshot())
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

// roadmapAndDone snapshots the persisted PlanState.Roadmap plus the
// set of wave labels already run, under a single lock. "Already run"
// = every Done wave label plus the in-flight Active wave's label
// (which covers the just-launched wave before it lands in Done — the
// same role the old code's `latestPlan.NextWave.Label` played).
//
// Phase 6.x — the roadmap source moved off the planner handoff: the
// fence-less planner has a body-less completion marker, so DecodePlan
// can no longer recover its roadmap. validate_and_approve writes the
// approved plan's roadmap to PlanState.Roadmap (SetRoadmap) and both
// roadmap readers snapshot it here.
func roadmapAndDone(m *MissionState) ([]RoadmapEntry, map[string]bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	roadmap := append([]RoadmapEntry(nil), m.Plan.Roadmap...)
	done := make(map[string]bool, len(m.Plan.Done)+1)
	for _, dw := range m.Plan.Done {
		if dw.Label != "" {
			done[dw.Label] = true
		}
	}
	if m.Plan.Active != nil && m.Plan.Active.Label != "" {
		done[m.Plan.Active.Label] = true
	}
	return roadmap, done
}

// collectPendingRoadmap reads the planner's latest committed roadmap
// (PlanState.Roadmap) and filters out entries whose `label` already
// ran — what's left is "the planner promised this and the mission
// hasn't done it yet". The checker sees this list and refuses
// `finish` when it isn't empty.
func collectPendingRoadmap(mission extension.SessionState) []plannerRoadmapView {
	m := FromState(mission)
	if m == nil {
		return nil
	}
	roadmap, done := roadmapAndDone(m)
	if len(roadmap) == 0 {
		return nil
	}
	out := make([]plannerRoadmapView, 0, len(roadmap))
	for _, r := range roadmap {
		if r.Label == "" || done[r.Label] {
			continue
		}
		out = append(out, plannerRoadmapView{Label: r.Label, Description: r.Description})
	}
	return out
}

// collectRoadmap projects the planner's latest committed roadmap
// (PlanState.Roadmap) into the planner's [Roadmap] view — EVERY
// entry, each flagged Done when a wave with that label already ran.
// Mirrors collectPendingRoadmap's source but keeps satisfied entries
// so the planner sees the full plan-ahead it committed to, not just
// the remainder. Empty when no approved plan carries a roadmap.
func collectRoadmap(mission extension.SessionState) []plannerRoadmapView {
	m := FromState(mission)
	if m == nil {
		return nil
	}
	roadmap, done := roadmapAndDone(m)
	if len(roadmap) == 0 {
		return nil
	}
	out := make([]plannerRoadmapView, 0, len(roadmap))
	for _, r := range roadmap {
		if r.Label == "" {
			continue
		}
		out = append(out, plannerRoadmapView{
			Label:       r.Label,
			Description: r.Description,
			Done:        done[r.Label],
		})
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
				switch {
				case s.TimedOut || s.Status == "timeout":
					// Distinct TIMEOUT signal — the worker overran its
					// time budget and was cancelled (it did not fail on
					// the work). The planner reacts by SPLITTING the task
					// into smaller parallel workers / sequential waves, or
					// REDOING it whole if it was nearly done.
					issues = append(issues, fmt.Sprintf("%s (role %s) TIMED OUT — it exceeded its time budget and was cancelled. SPLIT the task into smaller parallel workers or sequential waves so each fits its budget; only REDO it whole if it was nearly finished.", s.Name, s.Role))
				case s.Status == "error" && s.Error != "":
					ref := s.Ref
					if ref == "" {
						ref, _ = MakeRef(s.Name, waveLabel)
					}
					// Point the planner at the failed worker's handoff so it
					// can mission:get_handoff(ref) for WHAT IT ACCOMPLISHED
					// before deciding how to amend.
					issues = append(issues, fmt.Sprintf("%s (role %s) failed: %s — mission:get_handoff(%q) for its partial output", s.Name, s.Role, s.Error, ref))
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
	// ResearchAvailable is true when the mission carries a non-empty
	// research findings projection (m.ResearchOutput()). Gates the
	// proximate `[Research]` pull-pointer in the contract template so
	// workers in a researched mission are told — in their first
	// message, not just the distal constitution — to read the
	// verified findings via mission:get_research before re-deriving.
	ResearchAvailable bool
	// RoleProse is the worker role's behavioral brief
	// (`sub_agents[].prompt`), rendered into the template's
	// `[Your role]` slot. Looked up by role name in the mission
	// skill's RolePrompts; a cross-skill recipe worker (Skill ≠
	// mission skill) resolves empty → bare universal template.
	// Phase B34.
	RoleProse string
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
	researchAvailable := false
	if m := FromState(mission); m != nil {
		if findings, _, _ := m.ResearchOutput(); strings.TrimSpace(findings) != "" {
			researchAvailable = true
		}
	}
	out := wave
	out.Subagents = make([]SubagentSpec, len(wave.Subagents))
	copy(out.Subagents, wave.Subagents)
	for i := range out.Subagents {
		view := workerContractView{
			ResearchAvailable: researchAvailable,
			RoleProse:         renderRoleProse(mission, manifest.RolePrompts[out.Subagents[i].Role]),
		}
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
