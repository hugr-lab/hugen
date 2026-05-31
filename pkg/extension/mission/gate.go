package mission

import (
	"context"
	"strings"

	"github.com/hugr-lab/hugen/pkg/extension"
	wsext "github.com/hugr-lab/hugen/pkg/extension/workspace"
)

// GateTurnFinalize implements [extension.TurnFinalizeGate] for the
// mission. It holds a mission subagent's turn open until it has
// produced what its role owes — replacing the old terminal fences and
// post-close checks that let a subagent end its turn empty.
//
// Two governed roles:
//
//   - Planner — submission-gated: the turn may retire only once the
//     plan is submitted-and-approved through mission:validate_and_approve
//     (the single channel, Phase 6.x). Verdict read from the staged
//     outcome on the parent MissionState — approved / aborted → allow;
//     refine / invalid / never-submitted → block with the matching
//     continuation.
//   - Do worker / checker / synthesizer — fence-gated: the turn may
//     retire only once the final text carries a parseable terminal
//     ```handoff``` block. A model that ended WITHOUT it (forgot, or a
//     thinking-model whose tokens all went to reasoning leaving empty
//     content) is re-prompted IN-SESSION to emit it, instead of wedging
//     the executor's waitForRefs on a ref that never lands.
//
// Every other session (root, non-mission) returns allow=true. Blocking
// is bounded by the runtime's maxFinalizeGateRetries backstop (past the
// cap the turn retires regardless); a subagent that still produced no
// handoff is then recorded as a failed wave outcome (OnChildFrame) so
// the mission fails cleanly rather than hanging.
func (e *Extension) GateTurnFinalize(ctx context.Context, state extension.SessionState, finalText string) (string, bool) {
	parent, ok := state.Parent()
	if !ok || parent == nil {
		return "", true // root / non-child — not a mission subagent
	}
	mState := FromState(parent)
	if mState == nil {
		return "", true
	}
	// Planner branch — submission-gated via validate_and_approve.
	if role := mState.PlannerRole(); role != "" && state.Role() == role {
		return e.gatePlannerFinalize(parent, mState, state)
	}
	// Researcher branch — fence-gated AND file-gated (Phase 6.x —
	// research→files). The research role owes both a parseable research
	// fence AND filled artifact files; the check hook validates the
	// files IN-SESSION so the researcher fixes them without losing its
	// discovery context (Option B), never re-spawned from scratch.
	if role := mState.ResearchRole(); role != "" && state.Role() == role {
		return e.gateResearchFinalize(ctx, parent, state, finalText)
	}
	// Worker / checker / synthesizer branch — fence-gated. Only a
	// registered mission subagent owes a terminal handoff.
	cur, known := mState.LookupWorker(state.SessionID())
	if !known {
		return "", true
	}
	if strings.TrimSpace(finalText) != "" {
		if _, err := ParseHandoff(finalText); err == nil {
			return "", true // a parseable handoff fence — let it retire
		}
	}
	// Re-inject the original task alongside the handoff shape: a worker
	// that did the work but lost its brief over a long turn (compaction)
	// needs both reminders to report instead of re-deriving.
	view := gateHandoffMissingView{Task: mState.WorkerTask(cur.Name)}
	return e.renderGate(parent, "mission/handoff_missing", view), false
}

// gateHandoffMissingView feeds the worker's original task back into the
// handoff_missing nudge.
type gateHandoffMissingView struct{ Task string }

// gateResearchFinalize is the researcher half of the gate (Phase 6.x —
// research→files). It holds the researcher's turn open until BOTH a
// parseable terminal fence AND a passing research check hook — the
// check validates the research/*.md artifact files the role wrote.
// On a failed check it re-prompts IN-SESSION with the hook's feedback
// so the researcher fixes the files without losing the discovery
// context a fresh re-spawn would throw away (Option B). It fails OPEN
// when the check hook can't run (misconfigured tool / no catalog) so a
// broken gate degrades to "proceed" rather than wedging the mission;
// the runtime's maxFinalizeGateRetries backstop caps the retries.
func (e *Extension) gateResearchFinalize(ctx context.Context, parent, state extension.SessionState, finalText string) (string, bool) {
	// 1. Require a parseable terminal fence first — a researcher that
	//    ended WITHOUT its research block is re-prompted to emit it.
	if trimmed := strings.TrimSpace(finalText); trimmed == "" {
		return e.renderGate(parent, "mission/handoff_missing", gateHandoffMissingView{}), false
	} else if _, err := ParseHandoff(trimmed); err != nil {
		return e.renderGate(parent, "mission/handoff_missing", gateHandoffMissingView{}), false
	}
	// 2. Run the research check hook against the files. No catalog /
	//    no check declared → nothing to validate, allow.
	manifest, err := e.catalog.LookupMission(ctx, state.Skill())
	if err != nil || manifest == nil || manifest.Stages.Research.Check == nil {
		return "", true
	}
	view := hookView{MissionSkill: manifest.SkillDir}
	if ws := wsext.FromState(state); ws != nil {
		view.MissionDir = ws.Dir()
	}
	out, herr := runMissionHook(ctx, state, *manifest.Stages.Research.Check, view)
	if herr != nil {
		e.logger.Warn("mission: research gate: check hook could not run; allowing finalize",
			"session", state.SessionID(), "err", herr)
		return "", true // fail-open
	}
	if out.Failed {
		e.emitMissionOp(parent, "research_check_failed",
			map[string]any{"session": state.SessionID(), "reason": out.Reason})
		return e.renderGate(parent, "mission/research_check_failed",
			gateResearchCheckView{Reason: strings.TrimSpace(out.Reason)}), false
	}
	return "", true
}

// gateResearchCheckView feeds the check hook's failure detail back
// into the research_check_failed nudge so the researcher knows which
// file / section to fix.
type gateResearchCheckView struct{ Reason string }

// gatePlannerFinalize is the planner half of the gate: it reads the
// staged validate_and_approve outcome on the mission state and decides
// whether the planner's turn may retire (approved / aborted) or must
// re-iterate (refine / invalid / never-submitted), feeding the matching
// continuation template back into the session.
func (e *Extension) gatePlannerFinalize(parent extension.SessionState, mState *MissionState, state extension.SessionState) (string, bool) {
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
		view := gateRefineView{Feedback: strings.TrimSpace(sub.refineText)}
		return e.renderGate(parent, "mission/planner_refine", view), false
	case fresh && !sub.valid:
		view := gateInvalidPlanView{Errors: cleanErrs(sub.errs)}
		return e.renderGate(parent, "mission/planner_invalid_plan", view), false
	default:
		return e.renderGate(parent, "mission/planner_no_plan", nil), false
	}
}

// gateRefineView feeds the user's refine guidance into planner_refine.
type gateRefineView struct{ Feedback string }

// gateInvalidPlanView feeds the validate_and_approve errors into
// planner_invalid_plan.
type gateInvalidPlanView struct{ Errors []string }

// cleanErrs trims and drops empty entries so the invalid-plan template
// renders a tidy bullet list (or the "no detail" fallback when empty).
func cleanErrs(errs []string) []string {
	out := make([]string, 0, len(errs))
	for _, e := range errs {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	return out
}

// renderGate renders a gate-continuation template against the mission
// session's prompts renderer. A missing renderer or render error logs
// and returns "" — the gate still blocks finalization; the bare
// re-iteration alone may nudge the model even without steer text.
func (e *Extension) renderGate(mission extension.SessionState, name string, data any) string {
	r := mission.Prompts()
	if r == nil {
		e.logger.Warn("mission: gate: no prompts renderer", "template", name)
		return ""
	}
	out, err := r.Render(name, data)
	if err != nil {
		e.logger.Warn("mission: gate: render failed", "template", name, "err", err)
		return ""
	}
	return out
}
