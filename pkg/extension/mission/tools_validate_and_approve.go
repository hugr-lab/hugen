package mission

import (
	"context"
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
//  2. When the gate decides this iteration needs user sign-off
//     (first plan ever in the mission, OR a worker handoff
//     requested reapproval, OR the planner itself set
//     `requires_reapproval: true` in the body), runs a
//     session:inquire(type=approval) on the MISSION session so the
//     user sees the typed plan, rationale, and roadmap. The user's
//     response is folded into the result envelope:
//       - approve     → `approved: true`
//       - refine TEXT → `approved: false, refine_text: TEXT`
//       - abort       → `approved: false, aborted: true`
//  3. On `approved=true` (explicit or implicit), flips the
//     mission's firstPlanApproved bit on and clears any pending
//     reapproval flag so subsequent iterations pass silently — as
//     long as the planner doesn't set `requires_reapproval: true`
//     and no worker waves `invalidates_plan_approval: true` again.
//
// Plan_complete iterations (next_wave=null) bypass the modal — the
// mission contract was approved earlier; the final
// `finish` decision is gated by AC satisfaction, not by a fresh
// user approval. When the iteration's policy doesn't require
// approval at all (Initial=skip), the same uniform path applies.
//
// Phase 5.x — B13 superseded the prior sha256 frame-hashing gate.
// Weak models routinely rewrote `mission_goal` / `acceptance_criteria`
// strings cosmetically between iterations, which re-opened the
// modal on every iteration even when the strategic contract was
// unchanged. The new gate is explicit: the planner SIGNALS when
// reapproval is needed, the runtime takes that signal at face
// value, and workers can force-reopen via the handoff body flag.
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
	// (next_wave=null). plan_complete bypasses the modal entirely
	// (the mission contract was approved on an earlier iteration;
	// the `finish` decision is AC-gated, not approval-gated).

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

	// Mission-level escape hatch — when the policy explicitly opts
	// out of approvals (Initial=skip), flip the approved bit and
	// return without inquiring. Used by automation / test missions.
	policy := mState.PlannerApproval()
	if !approvalRequiredForIteration(policy, 0, mState) {
		mState.MarkPlanApproved()
		e.emitPlanApproved(parent, planApprovedPayload{Trigger: "policy_skip"})
		return emitValidateResult(validateResult{Approved: true})
	}

	// plan_complete bypasses the modal — finish is AC-gated
	// downstream, not approval-gated here. The mission's prior
	// approval still stands.
	if plan == nil {
		return emitValidateResult(validateResult{Approved: true})
	}

	if !shouldOpenApprovalModal(mState, plan) {
		return emitValidateResult(validateResult{Approved: true})
	}

	// Open the modal. Build the inquiry payload from the typed plan
	// body so the user reads the same contract the runtime will
	// commit to.
	pendingReason := ""
	if pending, reason := mState.PendingReapproval(); pending {
		pendingReason = reason
	}
	question, qErr := renderApprovalQuestion(parent, *plan)
	if qErr != nil {
		return toolErr("internal", "render approval question: "+qErr.Error())
	}
	resp, inqErr := parent.RequestInquiry(ctx, protocol.InquiryRequestPayload{
		Type:     protocol.InquiryTypeApproval,
		Question: question,
		Context:  approvalContextFor(*plan, pendingReason),
	})
	if inqErr != nil {
		return toolErr("inquire_failed", inqErr.Error())
	}
	if resp == nil {
		return toolErr("inquire_failed", "nil response from inquire")
	}
	approved, refine, aborted, reason := interpretValidateApprovalResponse(resp)
	if approved {
		mState.MarkPlanApproved()
		e.emitPlanApproved(parent, planApprovedPayload{Trigger: "user_modal", Reason: reason})
		return emitValidateResult(validateResult{
			Approved: true,
			Reason:   reason,
		})
	}
	return emitValidateResult(validateResult{
		Approved:   false,
		Aborted:    aborted,
		RefineText: refine,
		Reason:     reason,
	})
}

// shouldOpenApprovalModal applies the B13 gate. Returns true when
// the modal must run for this iteration. Inputs are the mission's
// current approval state + the planner's typed plan body.
//
// Order of checks matches the spec §4.3 invariant:
//
//  1. First plan ever in the mission → always modal. The user has
//     not signed off on anything yet, so the contract must surface.
//  2. A worker handoff requested reapproval since the last modal
//     closed → modal regardless of the planner's own flag. The
//     runtime trusts the worker's signal that something material
//     changed.
//  3. The planner itself set `requires_reapproval: true` in this
//     body → modal. Weak-model guidance: planner sets it ONLY when
//     `mission_goal` or `mission_acceptance_criteria` materially
//     changed vs the previously approved iteration.
//
// Otherwise the call passes silently — the prior approval stands.
func shouldOpenApprovalModal(m *MissionState, plan *Plan) bool {
	if !m.IsPlanApproved() {
		return true
	}
	if pending, _ := m.PendingReapproval(); pending {
		return true
	}
	if plan != nil && plan.RequiresReapproval {
		return true
	}
	return false
}

// approvalContextFor builds the InquiryRequestPayload.Context
// string for the approval modal. Carries the plan's rationale +
// (when reapproval was triggered by something other than first-
// iteration) a one-line explanation so the user knows why they're
// seeing the modal a second time. Order: planner-set
// ReapprovalReason wins over the worker-supplied pending reason —
// the planner is the authority on what changed strategically;
// the worker only signalled "something needs re-look".
func approvalContextFor(plan Plan, pendingReason string) string {
	rationale := strings.TrimSpace(plan.Rationale)
	reason := strings.TrimSpace(plan.ReapprovalReason)
	if reason == "" {
		reason = strings.TrimSpace(pendingReason)
	}
	if reason == "" {
		return rationale
	}
	if rationale == "" {
		return "Re-approval requested: " + reason
	}
	return rationale + "\n\nRe-approval requested: " + reason
}

// planApprovedPayload is the body of the `mission:plan_approved`
// ExtensionFrame. Recorded on every transition into the
// firstPlanApproved=true state so an eventual Recovery
// implementation can replay the bit after restart instead of
// re-opening the approval modal on the next iteration. Today no
// Recovery hook reads it — the frame is audit-only and a forward-
// compatible stash. See `mission-research-and-approval.md` §10
// (open question on approval state recovery).
type planApprovedPayload struct {
	// Trigger names which branch set the bit:
	//   "policy_skip" — manifest opted out of approvals.
	//   "user_modal"  — user clicked approve on the modal.
	Trigger string `json:"trigger"`
	// Reason carries the user's free-text reason (if any) from
	// the approve-with-reason path. Empty for policy_skip.
	Reason string `json:"reason,omitempty"`
}

// emitPlanApproved publishes the mission:plan_approved ExtensionFrame
// so the bit can be reconstructed on a future restart.
func (e *Extension) emitPlanApproved(mission extension.SessionState, payload planApprovedPayload) {
	e.emitMissionOp(mission, "plan_approved", payload)
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
// weak models latch onto the right key. Phase 5.x — B13 dropped
// the `plan_marker` field; the runtime no longer hashes plan
// bodies.
type validateResult struct {
	Valid      bool     `json:"valid"`
	Errors     []string `json:"errors,omitempty"`
	Approved   bool     `json:"approved,omitempty"`
	Aborted    bool     `json:"aborted,omitempty"`
	RefineText string   `json:"refine_text,omitempty"`
	Reason     string   `json:"reason,omitempty"`
}

func emitValidateResult(r validateResult) (json.RawMessage, error) {
	r.Valid = len(r.Errors) == 0
	return json.Marshal(r)
}
