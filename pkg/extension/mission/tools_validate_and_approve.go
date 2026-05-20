package mission

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// callValidateAndApprove is the planner role's commit checkpoint —
// the single atomic step the planner MUST run before emitting its
// fenced ```plan``` block when the iteration's approval policy
// demands user approval. It does three things in one call:
//
//  1. Validates the plan body via the same output_contract checks
//     the runtime would run post-close. Failure surfaces as
//     `{ valid: false, errors: [...] }` — the planner fixes the
//     listed issues and re-calls.
//  2. When the iteration's policy requires approval AND the plan
//     is NOT research-only, runs a session:inquire(type=approval)
//     on the MISSION session so the user sees the typed plan,
//     rationale, and roadmap. The user's response is folded into
//     the result envelope:
//       - approve     → `approved: true`
//       - refine TEXT → `approved: false, refine_text: TEXT`
//       - abort       → `approved: false, aborted: true`
//  3. On `approved=true` (explicit or implicit), computes the
//     canonical marker (sha256-hex of the typed Plan re-marshalled
//     as deterministic JSON) and stamps it on MissionState under
//     the current iteration. The runtime then verifies the
//     planner's handoff body produces the same marker in
//     spawnAndAwaitPlanner; mismatch rejects the handoff so a
//     planner cannot show plan A to the user and ship plan B.
//
// Research-only iterations (Phase I.15) skip the inquire — the
// tool still validates + stamps the marker so the verification
// path is uniform. When the iteration's policy doesn't require
// approval at all, same uniform path applies.
//
// Refuses a re-approval attempt for an already-dispatched
// iteration: `{ valid: true, errors: ["iteration N already
// dispatched..."] }` — the planner's recourse is to wait for the
// next iteration spawn.
func (e *Extension) callValidateAndApprove(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	state, ok := extension.SessionStateFromContext(ctx)
	if !ok || state == nil {
		return toolErr("session_gone", "no session attached to dispatch ctx")
	}
	var in validateAndApproveInput
	if err := json.Unmarshal(args, &in); err != nil {
		return toolErr("bad_request", fmt.Sprintf("invalid mission:validate_and_approve args: %v", err))
	}
	if len(in.Body) == 0 {
		return toolErr("bad_request", "body is required")
	}

	// Parse body as a generic object — the output_contract layer
	// expects `body` typed as `any` (map for kind=plan, string for
	// kind=handoff).
	var raw any
	if err := json.Unmarshal(in.Body, &raw); err != nil {
		return emitValidateResult(validateResult{Errors: []string{
			fmt.Sprintf("body is not valid JSON: %v", err),
		}})
	}

	candidate := Handoff{
		Kind:   KindPlan,
		Status: "ok",
		Body:   raw,
	}
	rawMap := map[string]any{"body": raw}
	var errs []string
	if err := validateRequired(KindPlan, candidate, rawMap); err != nil {
		errs = append(errs, err.Error())
	}
	plan, decodeErr := DecodePlan(candidate)
	if len(errs) == 0 && decodeErr != nil {
		errs = append(errs, decodeErr.Error())
	}
	if len(errs) > 0 {
		return emitValidateResult(validateResult{Errors: errs})
	}
	// Note: plan == nil is legitimate — it's the plan_complete shape
	// (next_wave=null). We still hash the body so the runtime
	// verifies the planner ships the same completion handoff it
	// asked approval for.

	// Resolve mission state via parent session — the planner runs
	// as a worker under the mission, and mission state lives on
	// the parent.
	parent, hasParent := state.Parent()
	if !hasParent || parent == nil {
		return toolErr("forbidden", "mission:validate_and_approve requires a planner session under a mission")
	}
	mState := FromState(parent)
	if mState == nil {
		return toolErr("unavailable", "mission state not initialised on the planner's parent")
	}

	// Compute the canonical marker over the decoded body. Go's
	// encoding/json emits map keys in sorted order and struct
	// fields in source order — both deterministic — so cosmetic
	// differences (whitespace, key ordering inside inputs maps)
	// collapse to the same digest. We hash the body, not the
	// typed Plan, so plan_complete (next_wave=null → nil Plan)
	// still produces a stable marker for the runtime to verify.
	marker, markerErr := canonicalPlanMarker(raw)
	if markerErr != nil {
		return toolErr("internal", "canonical plan marker: "+markerErr.Error())
	}

	// Mission-level escape hatch — when the policy explicitly
	// opts out of approvals (Initial=skip), stamp the marker
	// without inquiring. Used by automation / test missions.
	policy := mState.PlannerApproval()
	if !approvalRequiredForIteration(policy, 0, mState) {
		mState.SetApprovedPlanMarker(marker)
		return emitValidateResult(validateResult{
			Approved:   true,
			PlanMarker: marker,
		})
	}

	// Idempotent re-validate — when the planner submits a body
	// whose canonical marker EXACTLY matches the mission's
	// currently-approved marker, the user already approved this
	// exact plan. Return approved=true without re-prompting. This
	// lets the planner re-run validate_and_approve on a
	// previously-approved body (e.g. a refine loop that
	// converged back to the original) without hammering the
	// user with redundant modals.
	if existing := mState.ApprovedPlanMarker(); existing != "" && existing == marker {
		return emitValidateResult(validateResult{
			Approved:   true,
			PlanMarker: marker,
		})
	}

	// New / changed plan — run the inquire. plan_complete (nil
	// plan) substitutes a stand-in so the rendered modal carries
	// a sensible "mission complete" cue rather than a blank
	// roadmap.
	approvalPlan := plan
	if approvalPlan == nil {
		approvalPlan = &Plan{Rationale: "Mission complete — no further waves."}
	}
	question, qErr := renderApprovalQuestion(parent, *approvalPlan)
	if qErr != nil {
		return toolErr("internal", "render approval question: "+qErr.Error())
	}
	resp, inqErr := parent.RequestInquiry(ctx, protocol.InquiryRequestPayload{
		Type:     protocol.InquiryTypeApproval,
		Question: question,
		Context:  strings.TrimSpace(approvalPlan.Rationale),
	})
	if inqErr != nil {
		return toolErr("inquire_failed", inqErr.Error())
	}
	if resp == nil {
		return toolErr("inquire_failed", "nil response from inquire")
	}
	approved, refine, aborted, reason := interpretValidateApprovalResponse(resp)
	if approved {
		mState.SetApprovedPlanMarker(marker)
		return emitValidateResult(validateResult{
			Approved:   true,
			PlanMarker: marker,
			Reason:     reason,
		})
	}
	return emitValidateResult(validateResult{
		Approved:   false,
		Aborted:    aborted,
		RefineText: refine,
		Reason:     reason,
	})
}

// canonicalPlanMarker hashes the MISSION FRAME extracted from a
// plan body — `mission_goal` + `mission_acceptance_criteria` —
// not the full body. Returns the lowercase sha256-hex digest.
//
// Phase I.27 motivation: hashing the full body re-triggered the
// approval modal on every iteration because next_wave / roadmap
// naturally change as the mission progresses, even when the
// strategic contract (goal + AC) is identical. Hashing only the
// frame means:
//
//   - Planner edits mission_goal or mission_acceptance_criteria →
//     marker mismatch → re-validate_and_approve → user sees the
//     new modal. (Planner explicitly signalling "the contract
//     changed" by editing the frame.)
//   - Planner only changes next_wave / roadmap → marker matches →
//     idempotent → no modal. (Routine execution of the approved
//     mission.)
//   - Worker emits `invalidates_plan_approval: true` → runtime
//     clears the stored marker → next planner re-validate → new
//     modal regardless of frame text.
//
// Plan_complete (nil plan) is supported: when both fields are
// empty / absent the marker collapses to the hash of an empty
// frame — fine for the no-frame-change case (the existing
// approval stands).
//
// Used by the tool (over the args body) and by
// spawnAndAwaitPlanner (over h.Body) — symmetric callers produce
// the same digest when both see the same frame text.
func canonicalPlanMarker(body any) (string, error) {
	if body == nil {
		return "", fmt.Errorf("nil plan body")
	}
	m, ok := body.(map[string]any)
	if !ok {
		// Non-map body (string handoffs, scalar test fixtures) —
		// fall through to hashing whatever was passed; preserves
		// the previous behaviour for callers that hand in a non-
		// plan body.
		buf, err := json.Marshal(body)
		if err != nil {
			return "", fmt.Errorf("marshal plan body: %w", err)
		}
		sum := sha256.Sum256(buf)
		return hex.EncodeToString(sum[:]), nil
	}
	frame := struct {
		Goal               string   `json:"mission_goal,omitempty"`
		AcceptanceCriteria []string `json:"mission_acceptance_criteria,omitempty"`
	}{}
	if g, ok := m["mission_goal"].(string); ok {
		frame.Goal = strings.TrimSpace(g)
	}
	if rawAC, ok := m["mission_acceptance_criteria"].([]any); ok {
		for _, e := range rawAC {
			if s, ok := e.(string); ok {
				frame.AcceptanceCriteria = append(frame.AcceptanceCriteria, strings.TrimSpace(s))
			}
		}
	}
	buf, err := json.Marshal(frame)
	if err != nil {
		return "", fmt.Errorf("marshal mission frame: %w", err)
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:]), nil
}

// interpretValidateApprovalResponse normalises an InquiryResponse
// into the (approved, refine, aborted, reason) tuple the
// validate_and_approve envelope surfaces. Free-form text that
// doesn't lead with `approve` / `refine` / `abort` is treated as
// refinement guidance — the planner reads it as feedback and
// adjusts the plan, then re-calls.
func interpretValidateApprovalResponse(resp *protocol.InquiryResponse) (approved bool, refine string, aborted bool, reason string) {
	if resp.Payload.Timeout {
		return false, "", false, "approval inquire timed out"
	}
	if resp.Payload.Approved != nil {
		if *resp.Payload.Approved {
			return true, "", false, strings.TrimSpace(resp.Payload.Reason)
		}
		r := strings.TrimSpace(resp.Payload.Reason)
		if r == "" {
			r = "user denied approval without a reason"
		}
		return false, "", true, r
	}
	free := strings.TrimSpace(resp.Payload.Response)
	if free == "" {
		return false, "", true, "user reply was empty — treating as abort"
	}
	lower := strings.ToLower(free)
	switch {
	case strings.HasPrefix(lower, "approve"):
		return true, "", false, strings.TrimSpace(free[len("approve"):])
	case strings.HasPrefix(lower, "abort"):
		return false, "", true, strings.TrimSpace(free[len("abort"):])
	case strings.HasPrefix(lower, "refine"):
		body := strings.TrimSpace(free[len("refine"):])
		if body == "" {
			body = "(user replied 'refine' without details — ask for specifics or restate your reading of the request before re-validating)"
		}
		return false, body, false, "refine"
	}
	return false, free, false, "free-form user reply"
}

// validateResult is the envelope mission:validate_and_approve emits.
// Field omitempty discipline: only the fields populated for the
// given branch surface to the model — keeps the JSON narrow so
// weak models latch onto the right key.
type validateResult struct {
	Valid      bool     `json:"valid"`
	Errors     []string `json:"errors,omitempty"`
	Approved   bool     `json:"approved,omitempty"`
	Aborted    bool     `json:"aborted,omitempty"`
	RefineText string   `json:"refine_text,omitempty"`
	Reason     string   `json:"reason,omitempty"`
	PlanMarker string   `json:"plan_marker,omitempty"`
}

func emitValidateResult(r validateResult) (json.RawMessage, error) {
	r.Valid = len(r.Errors) == 0
	return json.Marshal(r)
}
