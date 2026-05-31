package mission

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// TestMaybeAutoApprove_GrantsWhenAncestorHasFlag is the happy path:
// caller is a worker session under a mission whose state has
// AutoApproveTools=true → the policy hook walks one parent up,
// finds the flag, returns (missionID, true) AND emits a
// mission:tool_approval_auto_granted ExtensionFrame on the granting
// mission session (NOT the caller).
func TestMaybeAutoApprove_GrantsWhenAncestorHasFlag(t *testing.T) {
	ext := newPlannerExtension()
	mission := newRenderedFakeState("mis-granting", productionRenderer(t))
	installMissionState(&mission.fakeState)
	mState := FromState(mission)
	if mState == nil {
		t.Fatal("FromState(mission) = nil")
	}
	mState.SetAutoApproveTools(true)

	worker := newRenderedFakeState("worker-1", productionRenderer(t))
	worker.fakeState.parent = mission

	gotMission, ok := ext.MaybeAutoApprove(context.Background(), worker, "bash-mcp:run")
	if !ok {
		t.Fatalf("MaybeAutoApprove ok=false; want true")
	}
	if gotMission != "mis-granting" {
		t.Errorf("granted mission id = %q, want %q", gotMission, "mis-granting")
	}
	if !findAutoGrantedFrame(t, mission.emittedFrames, "bash-mcp:run", "worker-1", "mis-granting") {
		t.Errorf("expected mission:tool_approval_auto_granted on granting mission; emitted=%v",
			frameKinds(mission.emittedFrames))
	}
}

// TestMaybeAutoApprove_GrantsOnResearchFlag covers the Phase 6.x
// research-stage path: the runtime sets AutoApproveResearch=true
// around the researcher wave so its workspace-internal bash.write_file
// calls don't open a modal. MaybeAutoApprove must honour it exactly
// like the user's AutoApproveTools pick.
func TestMaybeAutoApprove_GrantsOnResearchFlag(t *testing.T) {
	ext := newPlannerExtension()
	mission := newRenderedFakeState("mis-research", productionRenderer(t))
	installMissionState(&mission.fakeState)
	mState := FromState(mission)
	mState.SetAutoApproveResearch(true) // research stage active; AutoApproveTools stays false

	researcher := newRenderedFakeState("researcher-1", productionRenderer(t))
	researcher.fakeState.parent = mission

	gotMission, ok := ext.MaybeAutoApprove(context.Background(), researcher, "bash-mcp:bash.write_file")
	if !ok {
		t.Fatalf("MaybeAutoApprove ok=false; want true under research auto-approve")
	}
	if gotMission != "mis-research" {
		t.Errorf("granted mission id = %q, want mis-research", gotMission)
	}

	// And once research exits (flag cleared) the same call is gated again.
	mState.SetAutoApproveResearch(false)
	if _, ok := ext.MaybeAutoApprove(context.Background(), researcher, "bash-mcp:bash.write_file"); ok {
		t.Errorf("MaybeAutoApprove granted after research flag cleared; want gated")
	}
}

// TestMaybeAutoApprove_NoGrantWhenFlagOff covers the negative path:
// the flag was never set (or was reset by a subsequent modal). The
// hook returns (zero, false) so the runtime falls through to the
// user-facing modal — no audit frame fires (no grant happened).
func TestMaybeAutoApprove_NoGrantWhenFlagOff(t *testing.T) {
	ext := newPlannerExtension()
	mission := newRenderedFakeState("mis-no-flag", productionRenderer(t))
	installMissionState(&mission.fakeState)
	// Do not set the flag — default zero (false) value.

	worker := newRenderedFakeState("worker-2", productionRenderer(t))
	worker.fakeState.parent = mission

	gotMission, ok := ext.MaybeAutoApprove(context.Background(), worker, "bash-mcp:run")
	if ok {
		t.Errorf("MaybeAutoApprove ok=true with flag off; want false")
	}
	if gotMission != "" {
		t.Errorf("missionID = %q on no-grant; want empty", gotMission)
	}
	if findAutoGrantedFrame(t, mission.emittedFrames, "bash-mcp:run", "worker-2", "mis-no-flag") {
		t.Errorf("audit frame fired despite no grant")
	}
}

// TestMaybeAutoApprove_WalksMultipleAncestors verifies the policy
// walks past the immediate parent — caller is a deep worker (e.g.
// data-analyst under a research role under a mission), and the
// mission ancestor sits two hops up. The walk must traverse the
// chain until it finds the flag.
func TestMaybeAutoApprove_WalksMultipleAncestors(t *testing.T) {
	ext := newPlannerExtension()
	mission := newRenderedFakeState("mis-deep", productionRenderer(t))
	installMissionState(&mission.fakeState)
	FromState(mission).SetAutoApproveTools(true)

	middle := newRenderedFakeState("middle-role", productionRenderer(t))
	middle.fakeState.parent = mission

	worker := newRenderedFakeState("deep-worker", productionRenderer(t))
	worker.fakeState.parent = middle

	gotMission, ok := ext.MaybeAutoApprove(context.Background(), worker, "duckdb-mcp:query")
	if !ok {
		t.Fatalf("MaybeAutoApprove ok=false for two-hop walk; want true")
	}
	if gotMission != "mis-deep" {
		t.Errorf("granted mission id = %q, want %q", gotMission, "mis-deep")
	}
}

// TestMaybeAutoApprove_NilCallerSafe verifies the contract: a nil
// caller (defensive guard) must not panic and must return
// (zero, false). The runtime's hook iteration in requestApproval
// could legitimately reach this with a half-built session
// (extension wiring order) — failing closed is the safe default.
func TestMaybeAutoApprove_NilCallerSafe(t *testing.T) {
	ext := newPlannerExtension()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("MaybeAutoApprove panicked on nil caller: %v", r)
		}
	}()
	gotMission, ok := ext.MaybeAutoApprove(context.Background(), nil, "any:tool")
	if ok || gotMission != "" {
		t.Errorf("nil caller should return (\"\", false); got (%q, %v)", gotMission, ok)
	}
}

// TestMaybeAutoApprove_CallerIsMissionItself verifies the
// caller-itself branch — the planner / synthesizer roles dispatch
// from the mission session directly, not a worker under it. The
// policy must consult the caller's own state too, not start the
// walk at Parent().
func TestMaybeAutoApprove_CallerIsMissionItself(t *testing.T) {
	ext := newPlannerExtension()
	mission := newRenderedFakeState("mis-self", productionRenderer(t))
	installMissionState(&mission.fakeState)
	FromState(mission).SetAutoApproveTools(true)

	gotMission, ok := ext.MaybeAutoApprove(context.Background(), mission, "bash-mcp:run")
	if !ok {
		t.Fatalf("self-mission caller should grant; got false")
	}
	if gotMission != "mis-self" {
		t.Errorf("granted mission id = %q, want self %q", gotMission, "mis-self")
	}
}

// findAutoGrantedFrame searches captured frames for the audit
// payload matching tool + caller + granting mission. Returns true
// on hit.
func findAutoGrantedFrame(t *testing.T, frames []protocol.Frame, tool, caller, mission string) bool {
	t.Helper()
	for _, f := range frames {
		ef, ok := f.(*protocol.ExtensionFrame)
		if !ok {
			continue
		}
		if ef.Payload.Extension != "mission" || ef.Payload.Op != "tool_approval_auto_granted" {
			continue
		}
		var body toolApprovalAutoGrantedPayload
		if err := json.Unmarshal(ef.Payload.Data, &body); err != nil {
			t.Fatalf("decode tool_approval_auto_granted body: %v", err)
		}
		if body.Tool == tool && body.CallerSessionID == caller && body.GrantedByMissionSessionID == mission {
			return true
		}
	}
	return false
}
