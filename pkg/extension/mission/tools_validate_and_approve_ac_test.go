package mission

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// TestValidateAndApprove_FirstIterAC_AddCommitsOnApprove covers the
// full B11 happy path: iter 1 planner emits `ac_add` for two new
// criteria, the modal opens (first plan), user approves → both rows
// land in state.AC with planner_iter_1 origin + ac-1 / ac-2 ids.
func TestValidateAndApprove_FirstIterAC_AddCommitsOnApprove(t *testing.T) {
	ext := newPlannerExtension()
	mission := newRenderedFakeState("mis-b11-ac-commit", productionRenderer(t))
	installMissionState(&mission.fakeState)
	mState := FromState(mission)
	if mState == nil {
		t.Fatal("FromState(mission) = nil")
	}
	mState.IterationCounter = 1
	mState.SetPlannerApproval(PlanApproval{Initial: "required", Iteration: "always"})
	approved := true
	mission.inquiryResp = &protocol.InquiryResponse{
		Payload: protocol.InquiryResponsePayload{Approved: &approved},
	}
	planner := newRenderedFakeState("mis-b11-ac-commit-planner", productionRenderer(t))
	planner.fakeState.parent = mission
	ctx := extension.WithSessionState(context.Background(), planner)
	body := `{
        "mission_goal": "Deliver discovery wave",
        "ac_add": [
          {"statement": "Discovery wave runs"},
          {"statement": "Schema list captured"}
        ],
        "next_wave": {"label": "discover", "subagents": [{"name": "w", "role": "schema-explorer", "task": "t"}]},
        "roadmap": [{"label": "analyse", "description": "next step"}],
        "rationale": "first wave"
    }`
	args, _ := json.Marshal(map[string]any{"body": json.RawMessage(body)})
	out, err := ext.Call(ctx, "mission:validate_and_approve", args)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var res validateAndApproveResult
	if uErr := json.Unmarshal(out, &res); uErr != nil {
		t.Fatalf("unmarshal: %v", uErr)
	}
	if !res.Valid || !res.Approved {
		t.Fatalf("want valid+approved, got %+v", res)
	}
	rows := mState.ACSnapshot()
	if len(rows) != 2 {
		t.Fatalf("state.AC has %d rows after commit, want 2", len(rows))
	}
	byID := indexByID(rows)
	if byID["ac-1"].Statement != "Discovery wave runs" {
		t.Errorf("ac-1 statement=%q", byID["ac-1"].Statement)
	}
	if byID["ac-2"].Statement != "Schema list captured" {
		t.Errorf("ac-2 statement=%q", byID["ac-2"].Statement)
	}
	if byID["ac-1"].Origin != PlannerOriginAt(1) {
		t.Errorf("ac-1 origin=%q, want planner_iter_1", byID["ac-1"].Origin)
	}
	if byID["ac-1"].AddedAtIter != 1 {
		t.Errorf("ac-1 AddedAtIter=%d, want 1", byID["ac-1"].AddedAtIter)
	}
	if pending := mState.PendingDiff(); pending != nil {
		t.Errorf("pending diff not cleared post-commit: %+v", pending)
	}
}

// TestValidateAndApprove_FirstIterAC_AddDiscardsOnReject verifies
// the reject path: state.AC stays empty when user denies the modal.
func TestValidateAndApprove_FirstIterAC_AddDiscardsOnReject(t *testing.T) {
	ext := newPlannerExtension()
	mission := newRenderedFakeState("mis-b11-ac-reject", productionRenderer(t))
	installMissionState(&mission.fakeState)
	mState := FromState(mission)
	mState.IterationCounter = 1
	mState.SetPlannerApproval(PlanApproval{Initial: "required", Iteration: "always"})
	denied := false
	mission.inquiryResp = &protocol.InquiryResponse{
		Payload: protocol.InquiryResponsePayload{Approved: &denied, Reason: "scope wrong"},
	}
	planner := newRenderedFakeState("mis-b11-ac-reject-planner", productionRenderer(t))
	planner.fakeState.parent = mission
	ctx := extension.WithSessionState(context.Background(), planner)
	body := `{
        "mission_goal": "Run X",
        "ac_add": [{"statement": "X delivered"}],
        "next_wave": {"label": "x", "subagents": [{"name": "w", "role": "r", "task": "t"}]},
        "roadmap": [],
        "rationale": "first wave"
    }`
	args, _ := json.Marshal(map[string]any{"body": json.RawMessage(body)})
	out, err := ext.Call(ctx, "mission:validate_and_approve", args)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var res validateAndApproveResult
	_ = json.Unmarshal(out, &res)
	if res.Approved {
		t.Fatal("want approved=false on reject")
	}
	if len(mState.ACSnapshot()) != 0 {
		t.Errorf("state.AC populated despite reject: %+v", mState.ACSnapshot())
	}
	if pending := mState.PendingDiff(); pending != nil {
		t.Errorf("pending diff not cleared post-reject: %+v", pending)
	}
}

// TestValidateAndApprove_StatusOnlyDiff_AppliesSilently covers the
// non-contract path: planner emits ac_update with status only on a
// subsequent iter → no modal opens (firstPlanApproved already set, no
// reapproval flag, no contract change in diff), but state.AC reflects
// the new status.
func TestValidateAndApprove_StatusOnlyDiff_AppliesSilently(t *testing.T) {
	ext := newPlannerExtension()
	mission := newRenderedFakeState("mis-b11-ac-status-only", productionRenderer(t))
	installMissionState(&mission.fakeState)
	mState := FromState(mission)
	mState.IterationCounter = 2
	mState.SetPlannerApproval(PlanApproval{Initial: "required", Iteration: "always"})
	mState.MarkPlanApproved()
	mState.SeedAC([]ACAddSpec{
		{Statement: "X delivered"},
		{Statement: "Y captured"},
	}, OriginManifest)

	planner := newRenderedFakeState("mis-b11-ac-status-only-planner", productionRenderer(t))
	planner.fakeState.parent = mission
	ctx := extension.WithSessionState(context.Background(), planner)
	body := `{
        "mission_goal": "Run X and Y",
        "ac_update": [
          {"id": "ac-1", "status": "satisfied", "evidence": "wave-1 produced X"}
        ],
        "next_wave": {"label": "wave-2", "subagents": [{"name": "w", "role": "r", "task": "t"}]},
        "roadmap": [],
        "rationale": "continue"
    }`
	args, _ := json.Marshal(map[string]any{"body": json.RawMessage(body)})
	out, err := ext.Call(ctx, "mission:validate_and_approve", args)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var res validateAndApproveResult
	_ = json.Unmarshal(out, &res)
	if !res.Valid || !res.Approved {
		t.Fatalf("status-only diff should be silent-approved, got %+v", res)
	}
	if len(mission.inquiryRequests) != 0 {
		t.Errorf("status-only diff should NOT open modal; got %d inquiry requests", len(mission.inquiryRequests))
	}
	rows := mState.ACSnapshot()
	by := indexByID(rows)
	if by["ac-1"].Status != ACSatisfied {
		t.Errorf("ac-1 status=%q, want satisfied", by["ac-1"].Status)
	}
	if by["ac-1"].LastEvidence != "wave-1 produced X" {
		t.Errorf("ac-1 evidence=%q", by["ac-1"].LastEvidence)
	}
	if by["ac-2"].Status != ACUnsatisfied {
		t.Errorf("ac-2 should still be unsatisfied, got %q", by["ac-2"].Status)
	}
}

// TestValidateAndApprove_ContractDiff_AutoPromotesModal verifies §3.2.1
// auto-promote: planner emits ac_update with statement rewrite WITHOUT
// requires_reapproval — modal opens anyway because the runtime
// recognises the contract change.
func TestValidateAndApprove_ContractDiff_AutoPromotesModal(t *testing.T) {
	ext := newPlannerExtension()
	mission := newRenderedFakeState("mis-b11-ac-auto-promote", productionRenderer(t))
	installMissionState(&mission.fakeState)
	mState := FromState(mission)
	mState.IterationCounter = 2
	mState.SetPlannerApproval(PlanApproval{Initial: "required", Iteration: "always"})
	mState.MarkPlanApproved()
	mState.SeedAC([]ACAddSpec{{Statement: "Original wording"}}, OriginManifest)
	approved := true
	mission.inquiryResp = &protocol.InquiryResponse{
		Payload: protocol.InquiryResponsePayload{Approved: &approved},
	}
	planner := newRenderedFakeState("mis-b11-ac-auto-promote-planner", productionRenderer(t))
	planner.fakeState.parent = mission
	ctx := extension.WithSessionState(context.Background(), planner)
	body := `{
        "mission_goal": "Run X",
        "ac_update": [{"id": "ac-1", "statement": "Rewritten wording"}],
        "next_wave": {"label": "wave-2", "subagents": [{"name": "w", "role": "r", "task": "t"}]},
        "roadmap": [],
        "rationale": "continue"
    }`
	args, _ := json.Marshal(map[string]any{"body": json.RawMessage(body)})
	out, err := ext.Call(ctx, "mission:validate_and_approve", args)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var res validateAndApproveResult
	_ = json.Unmarshal(out, &res)
	if !res.Valid || !res.Approved {
		t.Fatalf("want valid+approved, got %+v", res)
	}
	if len(mission.inquiryRequests) != 1 {
		t.Fatalf("contract diff should open modal (auto-promote); got %d inquiry requests", len(mission.inquiryRequests))
	}
	rows := mState.ACSnapshot()
	if len(rows) != 1 || rows[0].Statement != "Rewritten wording" {
		t.Errorf("ac-1 should be rewritten after approve; got %+v", rows)
	}
}

// TestValidateAndApprove_PolicySkip_AppliesACWithoutModal covers the
// Initial=skip path: skill manifest opts out of approvals, but the
// planner's ac_add still applies — just without a modal.
func TestValidateAndApprove_PolicySkip_AppliesACWithoutModal(t *testing.T) {
	ext := newPlannerExtension()
	mission := newRenderedFakeState("mis-b11-ac-policy-skip", productionRenderer(t))
	installMissionState(&mission.fakeState)
	mState := FromState(mission)
	mState.IterationCounter = 1
	mState.SetPlannerApproval(PlanApproval{Initial: ApprovalInitialSkip})
	planner := newRenderedFakeState("mis-b11-ac-policy-skip-planner", productionRenderer(t))
	planner.fakeState.parent = mission
	ctx := extension.WithSessionState(context.Background(), planner)
	body := `{
        "mission_goal": "Run X",
        "ac_add": [{"statement": "X delivered"}],
        "next_wave": {"label": "wave-1", "subagents": [{"name": "w", "role": "r", "task": "t"}]},
        "roadmap": [],
        "rationale": "first wave"
    }`
	args, _ := json.Marshal(map[string]any{"body": json.RawMessage(body)})
	out, err := ext.Call(ctx, "mission:validate_and_approve", args)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var res validateAndApproveResult
	_ = json.Unmarshal(out, &res)
	if !res.Valid || !res.Approved {
		t.Fatalf("policy_skip should silent-approve, got %+v", res)
	}
	if len(mission.inquiryRequests) != 0 {
		t.Errorf("policy_skip should NOT open modal; got %d inquiry requests", len(mission.inquiryRequests))
	}
	rows := mState.ACSnapshot()
	if len(rows) != 1 || rows[0].Statement != "X delivered" {
		t.Errorf("AC should be applied under policy_skip; got %+v", rows)
	}
}

// TestValidateAndApprove_InvalidACDiff_RejectsAtParse verifies that
// malformed ac_add / ac_update entries surface as `valid: false` so
// the planner can self-correct without staging anything.
func TestValidateAndApprove_InvalidACDiff_RejectsAtParse(t *testing.T) {
	ext := newPlannerExtension()
	mission := newRenderedFakeState("mis-b11-ac-invalid", productionRenderer(t))
	installMissionState(&mission.fakeState)
	mState := FromState(mission)
	mState.IterationCounter = 1
	mState.SetPlannerApproval(PlanApproval{Initial: "required", Iteration: "always"})
	planner := newRenderedFakeState("mis-b11-ac-invalid-planner", productionRenderer(t))
	planner.fakeState.parent = mission
	ctx := extension.WithSessionState(context.Background(), planner)
	body := `{
        "mission_goal": "Run X",
        "ac_add": [{"statement": ""}],
        "next_wave": {"label": "wave-1", "subagents": [{"name": "w", "role": "r", "task": "t"}]},
        "roadmap": [],
        "rationale": "first wave"
    }`
	args, _ := json.Marshal(map[string]any{"body": json.RawMessage(body)})
	out, err := ext.Call(ctx, "mission:validate_and_approve", args)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var res validateAndApproveResult
	_ = json.Unmarshal(out, &res)
	if res.Valid {
		t.Fatalf("empty ac_add[0].statement should fail validation; got %+v", res)
	}
	if !containsAny(res.Errors, "ac_add") {
		t.Errorf("error should mention ac_add: %v", res.Errors)
	}
	if pending := mState.PendingDiff(); pending != nil {
		t.Errorf("pending diff stamped on validation failure: %+v", pending)
	}
}
