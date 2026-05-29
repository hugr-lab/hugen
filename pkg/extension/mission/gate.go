package mission

import (
	"context"
	"strings"

	"github.com/hugr-lab/hugen/pkg/extension"
)

// GateTurnFinalize implements [extension.TurnFinalizeGate] for the
// mission planner. It holds the planner's turn open until the plan is
// submitted-and-approved through mission:validate_and_approve (the
// single submission channel, Phase 6.x) — replacing the old terminal
// ```plan``` fence and the post-close approval check that let a
// planner end its turn with a valid:false plan.
//
// The gate governs ONLY the active planner child session — recognised
// by state.Role() == the parent mission's plan.role. Every other
// session (workers, checker, synthesizer, root, non-mission) returns
// allow=true and retires normally.
//
// Verdict (read from the staged validate_and_approve outcome on the
// parent MissionState):
//
//	approved (incl. plan_complete / silent / policy-skip) → allow=true
//	  → the turn retires; spawnAndAwaitPlanner reads the staged plan.
//	user aborted the approval modal                       → allow=true
//	  → the turn retires; the planner loop ends the mission as
//	    user_cancel (it reads submission.aborted), NOT a generic abort.
//	user asked to refine                                  → block, the
//	  continuation is the refine text; the planner reworks in-session.
//	validate returned valid:false                         → block, the
//	  continuation is the exact validation errors; the planner fixes
//	  the body and re-calls.
//	never called this turn (or a stale prior submission)  → block, the
//	  continuation demands a validate_and_approve call.
//
// Blocking is bounded by the session's maxFinalizeGateRetries backstop
// (the runtime stops consulting the gate past the cap and retires the
// turn) so a planner that never produces an approved plan falls
// through to the planner loop's own consecutive-error abort path.
func (e *Extension) GateTurnFinalize(_ context.Context, state extension.SessionState) (string, bool) {
	parent, ok := state.Parent()
	if !ok || parent == nil {
		return "", true // root / non-child — not a planner
	}
	mState := FromState(parent)
	if mState == nil {
		return "", true
	}
	role := mState.PlannerRole()
	if role == "" || state.Role() != role {
		return "", true // not the mission's planner session
	}

	sub := mState.PlannerSubmission()
	// Freshness: the staged outcome must belong to THIS planner turn.
	// A submission from a prior iteration (or none yet) is not fresh —
	// treat it as "never submitted this turn".
	fresh := sub.called && sub.sessionID == state.SessionID()

	switch {
	case fresh && sub.aborted:
		// User cancelled the plan — let the turn retire; the planner
		// loop terminates the mission as user_cancel.
		return "", true
	case fresh && sub.approved:
		// Approved (or plan_complete) — the runtime has a plan to run.
		return "", true
	case fresh && sub.refineText != "":
		return refineContinuation(sub.refineText), false
	case fresh && !sub.valid:
		return invalidPlanContinuation(sub.errs), false
	default:
		return neverSubmittedContinuation(), false
	}
}

// neverSubmittedContinuation is the gate's nudge when the planner
// tried to end its turn without ever calling validate_and_approve
// (this turn). Keeps the planner from closing on a bare "done".
func neverSubmittedContinuation() string {
	return "You have not submitted a plan yet. Do not end your turn. " +
		"Build the plan and submit it by calling mission:validate_and_approve(body=<the plan object>). " +
		"While it returns valid:false, fix the listed errors and call it again. " +
		"Once it returns valid:true (and, when approval is required, the user approves), end your turn with `done`."
}

// invalidPlanContinuation feeds the exact validation errors from the
// last validate_and_approve verdict back to the planner so it fixes
// the body and re-calls — instead of closing on an invalid plan.
func invalidPlanContinuation(errs []string) string {
	var b strings.Builder
	b.WriteString("Your last mission:validate_and_approve call returned valid:false. ")
	b.WriteString("Do not end your turn. Fix these errors in the plan body and call mission:validate_and_approve again until it returns valid:true:")
	if len(errs) == 0 {
		b.WriteString("\n- (no error detail was returned — re-check the plan body against the output contract)")
		return b.String()
	}
	for _, e := range errs {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		b.WriteString("\n- ")
		b.WriteString(e)
	}
	return b.String()
}

// refineContinuation feeds the user's refine guidance back to the
// planner so it revises the plan in-session and re-submits.
func refineContinuation(refine string) string {
	refine = strings.TrimSpace(refine)
	msg := "The user reviewed your plan and asked you to refine it. Do not end your turn. " +
		"Revise the plan to address their feedback, then call mission:validate_and_approve again."
	if refine != "" {
		msg += "\n\nUser feedback: " + refine
	}
	return msg
}
