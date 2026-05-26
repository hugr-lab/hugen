package mission

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// callApprovalModalForAutoApprove drives one approval modal through
// validate_and_approve and returns the (mState, mission, result)
// trio so tests can assert on state.AutoApproveTools, the emitted
// audit frames, and the tool result envelope in one place.
//
// approveWithTools controls the response payload's AutoApproveTools
// field. approveOK controls whether the user approves at all (set
// false to exercise the reject path).
func callApprovalModalForAutoApprove(t *testing.T, approveOK, approveWithTools bool, preflightInit func(m *MissionState)) (
	*MissionState, *renderedFakeState, validateAndApproveResult,
) {
	t.Helper()
	ext := newPlannerExtension()
	mission := newRenderedFakeState("mis-auto-approve", productionRenderer(t))
	installMissionState(&mission.fakeState)
	mState := FromState(mission)
	if mState == nil {
		t.Fatal("FromState(mission) = nil")
	}
	mState.IterationCounter = 1
	mState.SetPlannerApproval(PlanApproval{Initial: "required", Iteration: "always"})
	if preflightInit != nil {
		preflightInit(mState)
	}
	mission.inquiryResp = &protocol.InquiryResponse{
		Payload: protocol.InquiryResponsePayload{
			Approved:         &approveOK,
			AutoApproveTools: approveWithTools,
		},
	}
	planner := newRenderedFakeState("mis-auto-approve-planner", productionRenderer(t))
	planner.fakeState.parent = mission
	ctx := extension.WithSessionState(context.Background(), planner)
	body := `{
        "mission_goal": "Deliver discovery wave",
        "ac_add": [{"statement": "Discovery wave runs"}],
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
	return mState, mission, res
}

// TestValidateAndApprove_AutoApproveTools_StampedOnApprove covers the
// happy path of §4.6: response.Approved=true + AutoApproveTools=true →
// MissionState.AutoApproveTools flips on, audit event emitted with
// the matching payload. Without this stamp the policy hook
// (§4.6.5) has nothing to consult and the "approve with tools"
// option degrades silently to plain approve.
func TestValidateAndApprove_AutoApproveTools_StampedOnApprove(t *testing.T) {
	mState, mission, res := callApprovalModalForAutoApprove(t, true, true, nil)
	if !res.Valid || !res.Approved {
		t.Fatalf("want valid+approved, got %+v", res)
	}
	if !mState.AutoApproveTools() {
		t.Errorf("AutoApproveTools should be true after approve-with-tools modal")
	}
	if !findPolicySetFrame(t, mission.emittedFrames, true) {
		t.Errorf("expected mission:tool_approval_policy_set frame with auto_approve_tools=true; emitted=%v",
			frameKinds(mission.emittedFrames))
	}
}

// TestValidateAndApprove_AutoApproveTools_NotStampedOnPlainApprove
// covers the plain-approve branch: response.Approved=true with
// AutoApproveTools omitted/false leaves the flag at its reset (false)
// value. Audit event still fires (every modal close emits one)
// carrying auto_approve_tools=false so the audit log reads
// truthfully.
func TestValidateAndApprove_AutoApproveTools_NotStampedOnPlainApprove(t *testing.T) {
	mState, mission, res := callApprovalModalForAutoApprove(t, true, false, nil)
	if !res.Valid || !res.Approved {
		t.Fatalf("want valid+approved, got %+v", res)
	}
	if mState.AutoApproveTools() {
		t.Errorf("plain approve must not stamp AutoApproveTools=true")
	}
	if !findPolicySetFrame(t, mission.emittedFrames, false) {
		t.Errorf("expected mission:tool_approval_policy_set frame with auto_approve_tools=false; emitted=%v",
			frameKinds(mission.emittedFrames))
	}
}

// TestValidateAndApprove_AutoApproveTools_ResetOnEveryModalOpen
// covers §4.6.6 lifecycle: a prior modal stamped the flag, the
// next modal opens — flag clears BEFORE the response is read. The
// only way the flag re-sets is if THIS modal's response also
// carries AutoApproveTools=true. Pre-set + plain approve = reset.
func TestValidateAndApprove_AutoApproveTools_ResetOnEveryModalOpen(t *testing.T) {
	mState, mission, res := callApprovalModalForAutoApprove(t, true, false, func(m *MissionState) {
		// Simulate a prior modal having stamped the flag.
		m.SetAutoApproveTools(true)
	})
	if !res.Approved {
		t.Fatalf("want approved, got %+v", res)
	}
	if mState.AutoApproveTools() {
		t.Errorf("plain approve on second modal must reset prior AutoApproveTools=true")
	}
	// Audit event reflects the reset state, not the prior value.
	if !findPolicySetFrame(t, mission.emittedFrames, false) {
		t.Errorf("expected mission:tool_approval_policy_set frame with auto_approve_tools=false; emitted=%v",
			frameKinds(mission.emittedFrames))
	}
}

// TestValidateAndApprove_AutoApproveTools_NotStampedOnReject covers
// the reject path: response.Approved=false leaves the flag at its
// reset state, no policy_set frame emits (we only stamp+emit on
// approve in this revision; reject's audit lives in plan_approved
// negation channel). The flag must NOT survive a denied modal.
func TestValidateAndApprove_AutoApproveTools_NotStampedOnReject(t *testing.T) {
	mState, mission, res := callApprovalModalForAutoApprove(t, false, true, func(m *MissionState) {
		m.SetAutoApproveTools(true)
	})
	if res.Approved {
		t.Fatal("rejected modal must not approve")
	}
	if mState.AutoApproveTools() {
		t.Errorf("rejected modal must leave AutoApproveTools at reset (false), got true")
	}
	// On reject we do NOT emit policy_set (would be a lie — no
	// policy was set; the user opted out of the modal entirely).
	for _, f := range mission.emittedFrames {
		if ef, ok := f.(*protocol.ExtensionFrame); ok && ef.Payload.Op == "tool_approval_policy_set" {
			t.Errorf("policy_set frame should not emit on reject; got %+v", ef.Payload)
		}
	}
}

// findPolicySetFrame scans the captured frames for a
// mission:tool_approval_policy_set ExtensionFrame and verifies its
// payload's auto_approve_tools field matches want.
func findPolicySetFrame(t *testing.T, frames []protocol.Frame, want bool) bool {
	t.Helper()
	for _, f := range frames {
		ef, ok := f.(*protocol.ExtensionFrame)
		if !ok {
			continue
		}
		if ef.Payload.Extension != "mission" || ef.Payload.Op != "tool_approval_policy_set" {
			continue
		}
		var body toolApprovalPolicySetPayload
		if err := json.Unmarshal(ef.Payload.Data, &body); err != nil {
			t.Fatalf("decode tool_approval_policy_set body: %v", err)
		}
		if body.AutoApproveTools == want {
			return true
		}
	}
	return false
}

// frameKinds returns a short summary string for failure messages.
func frameKinds(frames []protocol.Frame) string {
	var b strings.Builder
	for _, f := range frames {
		if ef, ok := f.(*protocol.ExtensionFrame); ok {
			b.WriteString(ef.Payload.Extension + ":" + ef.Payload.Op + " ")

			continue
		}
		b.WriteString(string(f.Kind()) + " ")
	}
	return b.String()
}
