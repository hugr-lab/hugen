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
	PlanMarker string   `json:"plan_marker,omitempty"`
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
	if res.PlanMarker == "" {
		t.Fatal("want plan_marker populated on implicit approve")
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

// TestValidateAndApprove_ApprovalRequired_Approve covers the
// inquire path: policy demands approval, user approves, the
// tool stamps the canonical marker on MissionState so the
// runtime gate accepts the planner's handoff.
func TestValidateAndApprove_ApprovalRequired_Approve(t *testing.T) {
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
	if res.PlanMarker == "" {
		t.Fatal("want plan_marker populated on approve")
	}
	if got := mState.ApprovedPlanMarker(); got != res.PlanMarker {
		t.Fatalf("ApprovedPlanMarker = %q, want %q", got, res.PlanMarker)
	}
	if len(mission.inquiryRequests) != 1 {
		t.Fatalf("inquiryRequests = %d, want 1", len(mission.inquiryRequests))
	}
	q := mission.inquiryRequests[0].Question
	if !strings.Contains(q, "discover") || !strings.Contains(q, "later") {
		t.Errorf("question missing plan body details: %q", q)
	}
}

// TestValidateAndApprove_ApprovalRequired_Deny covers the inquire
// deny path: approved=false + aborted=true + reason; no marker is
// stamped so the runtime gate rejects any handoff the planner
// might still emit.
func TestValidateAndApprove_ApprovalRequired_Deny(t *testing.T) {
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
	if got := mState.ApprovedPlanMarker(); got != "" {
		t.Fatalf("ApprovedPlanMarker = %q, want empty (denial should not stamp)", got)
	}
}

// TestValidateAndApprove_IdempotentSameBody covers the
// already-approved short-circuit: when the planner re-submits a
// body whose marker matches the mission's currently-approved
// marker, the tool returns approved=true without re-inquiring.
func TestValidateAndApprove_IdempotentSameBody(t *testing.T) {
	ext := newPlannerExtension()
	mission := newRenderedFakeState("mis-vna-idem", productionRenderer(t))
	installMissionState(&mission.fakeState)
	mState := FromState(mission)
	if mState == nil {
		t.Fatal("FromState(mission) = nil")
	}
	planner := newRenderedFakeState("mis-vna-idem-planner", productionRenderer(t))
	planner.fakeState.parent = mission
	ctx := extension.WithSessionState(context.Background(), planner)

	body := map[string]any{
		"mission_goal":                "idempotent test",
		"mission_acceptance_criteria": []any{"discover wave runs"},
		"next_wave": map[string]any{
			"label":     "discover",
			"subagents": []any{map[string]any{"name": "w", "role": "researcher", "task": "t"}},
		},
		"roadmap":   []any{map[string]any{"label": "later", "description": "follow up"}},
		"rationale": "idempotent",
	}
	marker, err := canonicalPlanMarker(body)
	if err != nil {
		t.Fatalf("canonicalPlanMarker: %v", err)
	}
	mState.SetApprovedPlanMarker(marker)

	bodyJSON, _ := json.Marshal(body)
	args, _ := json.Marshal(map[string]any{"body": json.RawMessage(bodyJSON)})
	out, callErr := ext.Call(ctx, "mission:validate_and_approve", args)
	if callErr != nil {
		t.Fatalf("Call: %v", callErr)
	}
	var res validateAndApproveResult
	if uErr := json.Unmarshal(out, &res); uErr != nil {
		t.Fatalf("unmarshal: %v", uErr)
	}
	if !res.Valid || !res.Approved {
		t.Fatalf("idempotent re-approve: want valid+approved, got %+v", res)
	}
	if res.PlanMarker != marker {
		t.Errorf("PlanMarker = %q, want %q", res.PlanMarker, marker)
	}
	if len(mission.inquiryRequests) != 0 {
		t.Errorf("idempotent re-approve should NOT inquire; got %d", len(mission.inquiryRequests))
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
