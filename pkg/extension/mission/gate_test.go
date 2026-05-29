package mission

import (
	"context"
	"strings"
	"testing"
)

// newPlannerChild builds a planner child state under a mission whose
// MissionState has the plan role stamped, mirroring the production
// spawn (state.Role() == plannerRole; Parent() → mission).
func newPlannerChild(t *testing.T, plannerSessionID string) (*Extension, *fakeState, *fakeState, *MissionState) {
	t.Helper()
	mission := newFakeState("mis-gate")
	m := NewMissionState()
	m.SetPlannerRole("planner")
	mission.SetValue(StateKey, m)

	child := newFakeState(plannerSessionID)
	child.role = "planner"
	child.parent = mission

	return newPlannerExtension(), child, mission, m
}

func TestGateTurnFinalize_NonPlannerSessions_Allow(t *testing.T) {
	e := newPlannerExtension()

	// Root (no parent).
	if _, allow := e.GateTurnFinalize(context.Background(), newFakeState("root")); !allow {
		t.Fatal("root session: want allow=true")
	}

	// A worker child whose role != the mission's plan role.
	_, _, mission, _ := newPlannerChild(t, "ses-planner")
	worker := newFakeState("ses-worker")
	worker.role = "data-analyst"
	worker.parent = mission
	if _, allow := e.GateTurnFinalize(context.Background(), worker); !allow {
		t.Fatal("non-planner worker: want allow=true")
	}
}

func TestGateTurnFinalize_NeverSubmitted_Blocks(t *testing.T) {
	e, child, _, _ := newPlannerChild(t, "ses-planner")
	cont, allow := e.GateTurnFinalize(context.Background(), child)
	if allow {
		t.Fatal("planner that never called validate_and_approve: want block (allow=false)")
	}
	if !strings.Contains(cont, "validate_and_approve") {
		t.Fatalf("continuation should demand the tool call, got %q", cont)
	}
}

func TestGateTurnFinalize_Approved_Allows(t *testing.T) {
	e, child, _, m := newPlannerChild(t, "ses-planner")
	m.setPlannerSubmission(plannerSubmission{
		sessionID: "ses-planner",
		called:    true,
		valid:     true,
		approved:  true,
		plan:      &Plan{},
	})
	if _, allow := e.GateTurnFinalize(context.Background(), child); !allow {
		t.Fatal("approved submission: want allow=true")
	}
}

func TestGateTurnFinalize_Aborted_Allows(t *testing.T) {
	e, child, _, m := newPlannerChild(t, "ses-planner")
	m.setPlannerSubmission(plannerSubmission{
		sessionID: "ses-planner",
		called:    true,
		valid:     true,
		aborted:   true,
		reason:    "user declined",
	})
	// Abort lets the turn retire (allow); the planner loop reads
	// aborted and ends the mission as a cancellation.
	if _, allow := e.GateTurnFinalize(context.Background(), child); !allow {
		t.Fatal("aborted submission: want allow=true (planner loop handles cancel)")
	}
}

func TestGateTurnFinalize_Invalid_BlocksWithErrors(t *testing.T) {
	e, child, _, m := newPlannerChild(t, "ses-planner")
	m.setPlannerSubmission(plannerSubmission{
		sessionID: "ses-planner",
		called:    true,
		valid:     false,
		errs:      []string{"output_contract: kind=plan requires body.roadmap"},
	})
	cont, allow := e.GateTurnFinalize(context.Background(), child)
	if allow {
		t.Fatal("invalid plan: want block (allow=false)")
	}
	if !strings.Contains(cont, "body.roadmap") {
		t.Fatalf("continuation should carry the validation errors, got %q", cont)
	}
}

func TestGateTurnFinalize_Refine_BlocksWithFeedback(t *testing.T) {
	e, child, _, m := newPlannerChild(t, "ses-planner")
	m.setPlannerSubmission(plannerSubmission{
		sessionID:  "ses-planner",
		called:     true,
		valid:      true,
		refineText: "split the report into two sections",
	})
	cont, allow := e.GateTurnFinalize(context.Background(), child)
	if allow {
		t.Fatal("refine: want block (allow=false)")
	}
	if !strings.Contains(cont, "two sections") {
		t.Fatalf("continuation should carry the refine feedback, got %q", cont)
	}
}

func TestGateTurnFinalize_StaleSubmission_Blocks(t *testing.T) {
	// A submission from a PRIOR planner iteration (different session
	// id) is not fresh for the current planner turn — the gate treats
	// it as "never submitted this turn" and blocks.
	e, child, _, m := newPlannerChild(t, "ses-planner-2")
	m.setPlannerSubmission(plannerSubmission{
		sessionID: "ses-planner-1", // a prior iteration's planner
		called:    true,
		valid:     true,
		approved:  true,
		plan:      &Plan{},
	})
	if _, allow := e.GateTurnFinalize(context.Background(), child); allow {
		t.Fatal("stale (prior-iteration) submission: want block (allow=false)")
	}
}
