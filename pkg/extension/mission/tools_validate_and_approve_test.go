package mission

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// validateAndApproveResult mirrors the tool's output envelope so
// tests can decode it without re-marshalling. Mirror of the
// production validateResult.
type validateAndApproveResult struct {
	Valid      bool     `json:"valid"`
	Errors     []string `json:"errors,omitempty"`
	Approved   bool     `json:"approved,omitempty"`
	Aborted    bool     `json:"aborted,omitempty"`
	RefineText string   `json:"refine_text,omitempty"`
	Reason     string   `json:"reason,omitempty"`
}

// callValidateForBody runs the tool from a planner session with a
// parent mission state pre-installed and iteration=1. The mission's
// approval policy is left empty, so the implicit-approve path
// fires (no inquire is run).
func callValidateForBody(t *testing.T, body string) validateAndApproveResult {
	t.Helper()
	ext := newPlannerExtension()
	mission := newRenderedFakeState("mis-validate", productionRenderer(t))
	installMissionState(&mission.fakeState)
	mState := FromState(mission)
	if mState == nil {
		t.Fatal("FromState(mission) = nil")
	}
	mState.IterationCounter = 1
	planner := newRenderedFakeState("mis-validate-planner", productionRenderer(t))
	planner.fakeState.parent = mission
	ctx := extension.WithSessionState(context.Background(), planner)
	args, err := json.Marshal(map[string]json.RawMessage{
		"body": json.RawMessage(body),
	})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	out, err := ext.Call(ctx, "mission:validate_and_approve", args)
	if err != nil {
		t.Fatalf("Call(mission:validate_and_approve): %v", err)
	}
	var res validateAndApproveResult
	if uErr := json.Unmarshal(out, &res); uErr != nil {
		t.Fatalf("unmarshal result %q: %v", string(out), uErr)
	}
	return res
}

func TestValidateAndApprove_ValidPlan(t *testing.T) {
	body := `{
        "mission_goal": "Test plan accepted",
        "mission_acceptance_criteria": ["Discovery wave runs"],
        "next_wave": {
          "label": "discover",
          "subagents": [{"name": "explorer", "role": "schema-explorer", "task": "find schemas"}]
        },
        "roadmap": [{"label": "analyse", "description": "aggregate the findings"}],
        "rationale": "start with discovery"
    }`
	res := callValidateForBody(t, body)
	if !res.Valid {
		t.Fatalf("want valid, got errors=%v", res.Errors)
	}
	if !res.Approved {
		t.Fatalf("zero-policy mission → implicit approve, got approved=false (%+v)", res)
	}
}

func TestValidateAndApprove_PlanComplete(t *testing.T) {
	body := `{
        "next_wave": null,
        "roadmap": [],
        "rationale": "prior wave satisfied the goal"
    }`
	res := callValidateForBody(t, body)
	if !res.Valid {
		t.Fatalf("want valid (plan_complete shape), got errors=%v", res.Errors)
	}
}

func TestValidateAndApprove_MissingRoadmap(t *testing.T) {
	body := `{
        "mission_goal": "test",
        "mission_acceptance_criteria": ["x"],
        "next_wave": {
          "label": "x",
          "subagents": [{"name": "w", "role": "r", "task": "t"}]
        },
        "rationale": "no roadmap"
    }`
	res := callValidateForBody(t, body)
	if res.Valid {
		t.Fatal("want invalid for missing roadmap")
	}
	if !containsAny(res.Errors, "roadmap") {
		t.Errorf("errors do not mention roadmap: %v", res.Errors)
	}
}

func TestValidateAndApprove_EmptySubagents(t *testing.T) {
	body := `{
        "mission_goal": "test",
        "mission_acceptance_criteria": ["x"],
        "next_wave": {"label": "x", "subagents": []},
        "roadmap": [],
        "rationale": "no workers"
    }`
	res := callValidateForBody(t, body)
	if res.Valid {
		t.Fatal("want invalid for empty subagents")
	}
	if !containsAny(res.Errors, "subagents") {
		t.Errorf("errors do not mention subagents: %v", res.Errors)
	}
}

func TestValidateAndApprove_MissingRationale(t *testing.T) {
	body := `{
        "next_wave": null,
        "roadmap": []
    }`
	res := callValidateForBody(t, body)
	if res.Valid {
		t.Fatal("want invalid for missing rationale")
	}
	if !containsAny(res.Errors, "rationale") {
		t.Errorf("errors do not mention rationale: %v", res.Errors)
	}
}

func TestValidateAndApprove_NotJSON(t *testing.T) {
	out, err := callValidateAndApproveRaw(t, `{"body": invalid}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(string(out), "bad_request") {
		t.Fatalf("expected bad_request envelope, got %q", string(out))
	}
}

func callValidateAndApproveRaw(t *testing.T, args string) (json.RawMessage, error) {
	t.Helper()
	ext := newPlannerExtension()
	state := newRenderedFakeState("mis-validate-raw", productionRenderer(t))
	ctx := extension.WithSessionState(context.Background(), state)
	return ext.Call(ctx, "mission:validate_and_approve", json.RawMessage(args))
}

// TestValidateAndApprove_FirstIterApprove covers the gate's first-
// iter path: policy demands approval, no plan has ever been
// approved, user approves → modal runs once, firstPlanApproved
// flips to true.
func TestValidateAndApprove_FirstIterApprove(t *testing.T) {
	ext := newPlannerExtension()
	mission := newRenderedFakeState("mis-vna-approve", productionRenderer(t))
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
	planner := newRenderedFakeState("mis-vna-approve-planner", productionRenderer(t))
	planner.fakeState.parent = mission
	ctx := extension.WithSessionState(context.Background(), planner)
	body := `{
        "mission_goal": "deliver discovery",
        "mission_acceptance_criteria": ["discover wave runs"],
        "next_wave": {"label": "discover", "subagents": [{"name": "w", "role": "schema-explorer", "task": "t"}]},
        "roadmap": [{"label": "later", "description": "follow up"}],
        "rationale": "start with discovery"
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
	if !res.Valid {
		t.Fatalf("want valid, got errors=%v", res.Errors)
	}
	if !res.Approved {
		t.Fatalf("want approved=true, got %+v", res)
	}
	if !mState.IsPlanApproved() {
		t.Fatal("IsPlanApproved = false after approve, want true")
	}
	if len(mission.inquiryRequests) != 1 {
		t.Fatalf("inquiryRequests = %d, want 1", len(mission.inquiryRequests))
	}
	q := mission.inquiryRequests[0].Question
	if !strings.Contains(q, "discover") || !strings.Contains(q, "later") {
		t.Errorf("question missing plan body details: %q", q)
	}
}

// TestValidateAndApprove_FirstIterDeny covers the first-iter deny
// path: user rejects, firstPlanApproved stays false so the runtime
// gate will reject any handoff the planner might still emit.
func TestValidateAndApprove_FirstIterDeny(t *testing.T) {
	ext := newPlannerExtension()
	mission := newRenderedFakeState("mis-vna-deny", productionRenderer(t))
	installMissionState(&mission.fakeState)
	mState := FromState(mission)
	if mState == nil {
		t.Fatal("FromState(mission) = nil")
	}
	mState.IterationCounter = 1
	mState.SetPlannerApproval(PlanApproval{Initial: "required", Iteration: "always"})
	denied := false
	mission.inquiryResp = &protocol.InquiryResponse{
		Payload: protocol.InquiryResponsePayload{Approved: &denied, Reason: "wrong scope"},
	}
	planner := newRenderedFakeState("mis-vna-deny-planner", productionRenderer(t))
	planner.fakeState.parent = mission
	ctx := extension.WithSessionState(context.Background(), planner)
	body := `{
        "mission_goal": "deliver",
        "mission_acceptance_criteria": ["x runs"],
        "next_wave": {"label": "x", "subagents": [{"name": "w", "role": "r", "task": "t"}]},
        "roadmap": [],
        "rationale": "v1"
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
	if !res.Valid {
		t.Fatalf("want valid (validation passed), got errors=%v", res.Errors)
	}
	if res.Approved {
		t.Fatal("want approved=false")
	}
	if !res.Aborted {
		t.Fatal("explicit deny should surface aborted=true")
	}
	if !strings.Contains(res.Reason, "wrong scope") {
		t.Errorf("reason missing user feedback: %q", res.Reason)
	}
	if mState.IsPlanApproved() {
		t.Fatal("IsPlanApproved = true after deny, want false")
	}
}

// TestValidateAndApprove_SubsequentIterSilent covers the B13 happy
// path: a second iteration with the same goal/AC and no
// requires_reapproval flag passes silently without re-opening the
// modal. Weak-model wording drift in mission_goal/AC stays out of
// the user's face.
func TestValidateAndApprove_SubsequentIterSilent(t *testing.T) {
	ext := newPlannerExtension()
	mission := newRenderedFakeState("mis-vna-silent", productionRenderer(t))
	installMissionState(&mission.fakeState)
	mState := FromState(mission)
	if mState == nil {
		t.Fatal("FromState(mission) = nil")
	}
	mState.IterationCounter = 2
	mState.SetPlannerApproval(PlanApproval{Initial: "required", Iteration: "always"})
	// Simulate "user already approved the initial plan on iter 1".
	mState.MarkPlanApproved()
	planner := newRenderedFakeState("mis-vna-silent-planner", productionRenderer(t))
	planner.fakeState.parent = mission
	ctx := extension.WithSessionState(context.Background(), planner)
	// Mild wording drift in mission_goal — must NOT re-open modal.
	body := `{
        "mission_goal": "Deliver the Q4 sales dashboard end-to-end",
        "mission_acceptance_criteria": ["HTML file saved", "Charts render client-side"],
        "next_wave": {"label": "wave-2", "subagents": [{"name": "w", "role": "data-analyst", "task": "t"}]},
        "roadmap": [],
        "rationale": "continue execution"
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
		t.Fatalf("subsequent iter same contract: want valid+approved, got %+v", res)
	}
	if len(mission.inquiryRequests) != 0 {
		t.Errorf("subsequent iter without requires_reapproval should NOT inquire; got %d", len(mission.inquiryRequests))
	}
}

// TestValidateAndApprove_RequiresReapprovalFlagReopens covers the
// B13 explicit signal: planner sets requires_reapproval:true → modal
// re-opens even though firstPlanApproved is on the bit.
func TestValidateAndApprove_RequiresReapprovalFlagReopens(t *testing.T) {
	ext := newPlannerExtension()
	mission := newRenderedFakeState("mis-vna-reapprove", productionRenderer(t))
	installMissionState(&mission.fakeState)
	mState := FromState(mission)
	if mState == nil {
		t.Fatal("FromState(mission) = nil")
	}
	mState.IterationCounter = 2
	mState.SetPlannerApproval(PlanApproval{Initial: "required", Iteration: "always"})
	mState.MarkPlanApproved()
	approved := true
	mission.inquiryResp = &protocol.InquiryResponse{
		Payload: protocol.InquiryResponsePayload{Approved: &approved},
	}
	planner := newRenderedFakeState("mis-vna-reapprove-planner", productionRenderer(t))
	planner.fakeState.parent = mission
	ctx := extension.WithSessionState(context.Background(), planner)
	body := `{
        "mission_goal": "Deliver a sales dashboard AND ingest pipeline",
        "mission_acceptance_criteria": ["HTML saved", "Charts render", "Ingest job scheduled"],
        "next_wave": {"label": "wave-2", "subagents": [{"name": "w", "role": "data-analyst", "task": "t"}]},
        "roadmap": [],
        "rationale": "scope expanded after refine-loop",
        "requires_reapproval": true,
        "reapproval_reason": "user expanded scope to include ingest pipeline"
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
		t.Fatalf("requires_reapproval+approve: want valid+approved, got %+v", res)
	}
	if len(mission.inquiryRequests) != 1 {
		t.Fatalf("requires_reapproval should open modal; got %d inquiry requests", len(mission.inquiryRequests))
	}
	q := mission.inquiryRequests[0]
	if !strings.Contains(q.Context, "ingest pipeline") {
		t.Errorf("context should surface reapproval_reason; got %q", q.Context)
	}
}

// TestValidateAndApprove_PendingReapprovalReopens covers the worker-
// triggered branch: a worker handoff requested reapproval since the
// last approve, so the next planner iteration must see the modal
// regardless of its own requires_reapproval flag.
func TestValidateAndApprove_PendingReapprovalReopens(t *testing.T) {
	ext := newPlannerExtension()
	mission := newRenderedFakeState("mis-vna-pending", productionRenderer(t))
	installMissionState(&mission.fakeState)
	mState := FromState(mission)
	if mState == nil {
		t.Fatal("FromState(mission) = nil")
	}
	mState.IterationCounter = 2
	mState.SetPlannerApproval(PlanApproval{Initial: "required", Iteration: "always"})
	mState.MarkPlanApproved()
	mState.RequestReapproval("worker discovered new scope")
	approved := true
	mission.inquiryResp = &protocol.InquiryResponse{
		Payload: protocol.InquiryResponsePayload{Approved: &approved},
	}
	planner := newRenderedFakeState("mis-vna-pending-planner", productionRenderer(t))
	planner.fakeState.parent = mission
	ctx := extension.WithSessionState(context.Background(), planner)
	// Note: requires_reapproval is NOT set on the body — the worker's
	// pending bit alone must trigger the modal.
	body := `{
        "mission_goal": "Updated goal after worker insight",
        "mission_acceptance_criteria": ["Updated criterion"],
        "next_wave": {"label": "wave-2", "subagents": [{"name": "w", "role": "data-analyst", "task": "t"}]},
        "roadmap": [],
        "rationale": "responding to worker findings"
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
	if !res.Approved {
		t.Fatalf("want approved=true, got %+v", res)
	}
	if len(mission.inquiryRequests) != 1 {
		t.Fatalf("pending_reapproval should open modal; got %d inquiries", len(mission.inquiryRequests))
	}
	if !strings.Contains(mission.inquiryRequests[0].Context, "worker discovered new scope") {
		t.Errorf("context should carry worker reason; got %q", mission.inquiryRequests[0].Context)
	}
	// After approve, both bits should be in the "cleared" state.
	if !mState.IsPlanApproved() {
		t.Fatal("IsPlanApproved = false after reapprove, want true")
	}
	if pending, _ := mState.PendingReapproval(); pending {
		t.Fatal("PendingReapproval still true after reapprove, want false")
	}
}

// TestValidateAndApprove_PlanCompleteBypassesModal covers the
// finish path: plan_complete (next_wave=null) must not re-prompt
// the user even when requires_reapproval was set — finish is
// AC-gated downstream, not approval-gated here.
func TestValidateAndApprove_PlanCompleteBypassesModal(t *testing.T) {
	ext := newPlannerExtension()
	mission := newRenderedFakeState("mis-vna-complete", productionRenderer(t))
	installMissionState(&mission.fakeState)
	mState := FromState(mission)
	if mState == nil {
		t.Fatal("FromState(mission) = nil")
	}
	mState.IterationCounter = 3
	mState.SetPlannerApproval(PlanApproval{Initial: "required", Iteration: "always"})
	mState.MarkPlanApproved()
	planner := newRenderedFakeState("mis-vna-complete-planner", productionRenderer(t))
	planner.fakeState.parent = mission
	ctx := extension.WithSessionState(context.Background(), planner)
	body := `{
        "next_wave": null,
        "roadmap": [],
        "rationale": "all AC satisfied"
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
	if !res.Approved {
		t.Fatalf("plan_complete should approve silently, got %+v", res)
	}
	if len(mission.inquiryRequests) != 0 {
		t.Errorf("plan_complete must not open modal; got %d inquiries", len(mission.inquiryRequests))
	}
}

func containsAny(haystack []string, needle string) bool {
	for _, h := range haystack {
		if strings.Contains(h, needle) {
			return true
		}
	}
	return false
}

// TestShouldOpenApprovalModal exercises the B13 gate decision table
// directly — proves only the documented triggers open the modal.
func TestShouldOpenApprovalModal(t *testing.T) {
	cases := []struct {
		name             string
		approved         bool
		pendingReason    string
		planRequiresFlag bool
		want             bool
	}{
		{"first plan ever", false, "", false, true},
		{"first plan ever even with flag", false, "", true, true},
		{"approved + no flag + no pending", true, "", false, false},
		{"approved + planner flag", true, "", true, true},
		{"approved + worker requested reapproval", true, "worker reason", false, true},
		{"approved + flag + worker reason", true, "worker reason", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMissionState()
			if tc.approved {
				m.MarkPlanApproved()
			}
			if tc.pendingReason != "" {
				m.RequestReapproval(tc.pendingReason)
			}
			plan := &Plan{RequiresReapproval: tc.planRequiresFlag}
			if got := shouldOpenApprovalModal(m, plan); got != tc.want {
				t.Errorf("shouldOpenApprovalModal = %v, want %v", got, tc.want)
			}
		})
	}
}
