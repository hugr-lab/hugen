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
//     - approve     → `approved: true`
//     - refine TEXT → `approved: false, refine_text: TEXT`
//     - abort       → `approved: false, aborted: true`
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
	// return without inquiring. AC diff still applies (no modal, so
	// status-only and contract-add land in one immediate commit).
	policy := mState.PlannerApproval()
	if !approvalRequiredForIteration(policy, 0, mState) {
		if plan != nil {
			if err := applyPlanACForPolicySkip(mState, *plan); err != nil {
				return toolErr("internal", "apply ac diff (policy_skip): "+err.Error())
			}
		}
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

	// Decide modal up front so the diff merge knows which path to
	// take. Status-only diffs that pass through the no-modal branch
	// still apply immediately — they're not contract changes.
	wantModal := shouldOpenApprovalModal(mState, plan)
	if !wantModal {
		if err := applyPlanACSilent(mState, *plan); err != nil {
			return toolErr("internal", "apply ac diff (silent): "+err.Error())
		}
		return emitValidateResult(validateResult{Approved: true})
	}

	// Modal needed. Stage the diff so the renderer can overlay the
	// proposed changes; the user sees the contract they're signing.
	// On approve, CommitStagedDiff folds the staged set into state.AC;
	// on refine/abort, DiscardStagedDiff drops it without mutating.
	stagedReason := strings.TrimSpace(plan.ReapprovalReason)
	if stagedReason == "" {
		diff := ACDiff{Add: plan.ACAdd, Update: plan.ACUpdate}
		if diff.HasContractChange() && !plan.RequiresReapproval {
			stagedReason = "planner emitted contract diff (auto-promoted to re-approval)"
		}
	}
	stageDiff := ACDiff{Add: plan.ACAdd, Update: plan.ACUpdate}
	if !stageDiff.IsEmpty() {
		if err := mState.StagePlannerDiff(stageDiff, mState.IterationCounter, PlannerOriginAt(mState.IterationCounter), stagedReason); err != nil {
			return toolErr("internal", "stage planner ac diff: "+err.Error())
		}
	}

	pendingReason := ""
	if pending, reason := mState.PendingReapproval(); pending {
		pendingReason = reason
	}
	question, qErr := renderApprovalQuestion(parent, *plan)
	if qErr != nil {
		mState.DiscardStagedDiff()
		return toolErr("internal", "render approval question: "+qErr.Error())
	}
	// §4.6.6 lifecycle — every fresh approval modal clears the
	// prior auto-approve-tools pick. The flag re-sets only when
	// THIS modal closes with AutoApproveTools=true (approve-with-
	// tools path). Refine / abort / approve-without-tools all
	// leave the flag in its reset (false) state.
	mState.SetAutoApproveTools(false)
	resp, inqErr := parent.RequestInquiry(ctx, protocol.InquiryRequestPayload{
		Type:     protocol.InquiryTypeApproval,
		Question: question,
		Context:  approvalContextFor(*plan, pendingReason),
	})
	if inqErr != nil {
		mState.DiscardStagedDiff()
		return toolErr("inquire_failed", inqErr.Error())
	}
	if resp == nil {
		mState.DiscardStagedDiff()
		return toolErr("inquire_failed", "nil response from inquire")
	}
	approved, refine, aborted, reason := interpretValidateApprovalResponse(resp)
	// §4.6.4 — emit the audit frame on EVERY modal close so the audit
	// log carries the full sequence of user picks per approval. The
	// `auto_approve_tools` field reads from the response payload for
	// the approve branch; refine / reject reset to false (the modal
	// didn't grant blanket auto-approval). The frame is default-deny
	// visibility — never reaches the model prompt.
	autoApproveTools := approved && resp.Payload.AutoApproveTools
	e.emitToolApprovalPolicySet(parent, toolApprovalPolicySetPayload{
		AutoApproveTools: autoApproveTools,
		Iteration:        mState.IterationCounter,
	})
	if approved {
		if _, err := mState.CommitStagedDiff(ACDiff{}); err != nil {
			return toolErr("internal", "commit staged ac diff: "+err.Error())
		}
		mState.MarkPlanApproved()
		if autoApproveTools {
			mState.SetAutoApproveTools(true)
		}
		e.emitPlanApproved(parent, planApprovedPayload{Trigger: "user_modal", Reason: reason})
		return emitValidateResult(validateResult{
			Approved: true,
			Reason:   reason,
		})
	}
	mState.DiscardStagedDiff()
	return emitValidateResult(validateResult{
		Approved:   false,
		Aborted:    aborted,
		RefineText: refine,
		Reason:     reason,
	})
}

// applyPlanACSilent applies the planner's diff immediately when the
// approval modal is not opening. Status-only ac_update entries pass
// through ApplyStatusOnly; ac_add entries (which always trigger
// contract change) should not reach this branch — shouldOpenApprovalModal
// auto-promotes contract diffs to modal. Defensive: error out if any
// contract change leaked here, so we never silently bypass approval
// on a contract change.
func applyPlanACSilent(m *MissionState, plan Plan) error {
	diff := ACDiff{Add: plan.ACAdd, Update: plan.ACUpdate}
	if diff.HasContractChange() {
		return fmt.Errorf("applyPlanACSilent invoked with contract change diff — shouldOpenApprovalModal must auto-promote")
	}
	if len(plan.ACUpdate) == 0 {
		return nil
	}
	return m.ApplyStatusOnly(plan.ACUpdate, m.IterationCounter, PlannerOriginAt(m.IterationCounter))
}

// applyPlanACForPolicySkip stages then immediately commits the
// planner's diff for missions whose approval policy is `skip`. No
// modal opens, so we honour the planner's diff verbatim — adds + all
// updates apply with the planner-iter origin.
func applyPlanACForPolicySkip(m *MissionState, plan Plan) error {
	diff := ACDiff{Add: plan.ACAdd, Update: plan.ACUpdate}
	if diff.IsEmpty() {
		return nil
	}
	if err := m.StagePlannerDiff(diff, m.IterationCounter, PlannerOriginAt(m.IterationCounter), "policy_skip — applied without modal"); err != nil {
		return err
	}
	_, err := m.CommitStagedDiff(ACDiff{})
	return err
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
	// B11 §3.2.1 auto-promote: contract diffs always open the modal,
	// even when the planner forgot the flag.
	if plan != nil {
		diff := ACDiff{Add: plan.ACAdd, Update: plan.ACUpdate}
		if diff.HasContractChange() {
			return true
		}
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

// toolApprovalPolicySetPayload is the body of the
// `mission:tool_approval_policy_set` ExtensionFrame — recorded on
// every approval-modal close (regardless of whether the user picked
// auto-approve) so the audit log carries the full sequence of "user
// picks per approval" over the mission's lifetime. Phase 5.x — §4.6.4.
type toolApprovalPolicySetPayload struct {
	// AutoApproveTools is the post-modal state of the flag.
	AutoApproveTools bool `json:"auto_approve_tools"`
	// Iteration is the planner iteration the modal closed on.
	Iteration int `json:"iteration"`
}

// emitToolApprovalPolicySet publishes the
// mission:tool_approval_policy_set ExtensionFrame. Default-deny in
// visibility filters — never reaches the model prompt; audit-only.
func (e *Extension) emitToolApprovalPolicySet(mission extension.SessionState, payload toolApprovalPolicySetPayload) {
	e.emitMissionOp(mission, "tool_approval_policy_set", payload)
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
