package mission

import (
	"context"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// Phase 5.2 budget-termination, Part 2 (Decision C) — mission-side
// handling of a child that crossed its hard context budget.

func budgetTerminatedFrame() *protocol.SessionTerminated {
	return &protocol.SessionTerminated{
		Payload: protocol.SessionTerminatedPayload{
			Reason: protocol.TerminationContextBudget,
		},
	}
}

func TestOnChildFrame_BudgetTermination_OrchestrationAborts(t *testing.T) {
	ext := NewExtension(Config{})
	state := newFakeState("mis-1")
	ext.InitState(context.Background(), state) // skipcq
	m := FromState(state)
	m.BeginWave(Wave{Label: "_check-1"})
	m.RegisterWorker("chk-1", workerCursor{Name: "checker", Role: "checker"})

	ext.OnChildFrame(context.Background(), state, "chk-1", budgetTerminatedFrame())

	// Failed handoff recorded (waitForRefs resolves cleanly).
	got, ok := m.Handoffs.Get("checker@_check-1")
	if !ok {
		t.Fatal("budget termination must record a failed handoff so the wave settles")
	}
	if got.Status != "error" || !strings.Contains(got.Reason, "context budget") {
		t.Fatalf("handoff = {%q, %q}, want error + 'context budget'", got.Status, got.Reason)
	}
	// Orchestration role → mission flagged for clean abort.
	if role, ok := m.BudgetAbortInfo(); !ok || role != "checker" {
		t.Fatalf("BudgetAbortInfo = (%q,%v), want (checker,true)", role, ok)
	}
}

func TestOnChildFrame_BudgetTermination_PlannerAbortsBeforeCompletion(t *testing.T) {
	ext := NewExtension(Config{})
	state := newFakeState("mis-1")
	ext.InitState(context.Background(), state) // skipcq
	m := FromState(state)
	m.BeginWave(Wave{Label: plannerWaveLabelPrefix + "1"})
	m.RegisterWorker("pln-1", workerCursor{Name: "planner", Role: "planner"})

	// The budget check runs BEFORE the planner-completion special case,
	// so a planner that ran out of budget aborts rather than recording
	// a "never submitted a plan" error and re-spawning.
	ext.OnChildFrame(context.Background(), state, "pln-1", budgetTerminatedFrame())

	if role, ok := m.BudgetAbortInfo(); !ok || role != "planner" {
		t.Fatalf("BudgetAbortInfo = (%q,%v), want (planner,true)", role, ok)
	}
}

func TestOnChildFrame_BudgetTermination_WorkerReplans(t *testing.T) {
	ext := NewExtension(Config{})
	state := newFakeState("mis-1")
	ext.InitState(context.Background(), state) // skipcq
	m := FromState(state)
	m.BeginWave(Wave{Label: "do-1"}) // a planner-chosen worker wave (not _-prefixed)
	m.RegisterWorker("wrk-1", workerCursor{Name: "analyst", Role: "data-analyst"})

	ext.OnChildFrame(context.Background(), state, "wrk-1", budgetTerminatedFrame())

	// Failed handoff recorded so the wave settles + re-plans...
	got, ok := m.Handoffs.Get("analyst@do-1")
	if !ok || got.Status != "error" {
		t.Fatalf("worker budget termination must record a failed handoff, got ok=%v %+v", ok, got)
	}
	// ...but a worker does NOT abort the whole mission.
	if role, ok := m.BudgetAbortInfo(); ok {
		t.Fatalf("worker budget termination must NOT flag a mission abort, got role=%q", role)
	}
}

func TestBuildFinalText_BudgetAbortRecap(t *testing.T) {
	state := newFakeState("mis-1")
	NewExtension(Config{}).InitState(context.Background(), state) // skipcq
	m := FromState(state)
	m.MarkBudgetAbort("researcher")

	got := buildFinalText(state, "", true)
	if !strings.Contains(got, "researcher") || !strings.Contains(got, "context budget") {
		t.Fatalf("recap = %q, want it to name the researcher + context budget", got)
	}
	if !strings.Contains(got, "preserved") {
		t.Fatalf("recap = %q, want it to mention preserved findings", got)
	}
}

// TestOnChildFrame_BudgetFinalize_ForcesError covers the primary
// path: the role emitted its summary handoff under a tools-disabled
// budget cut (runtime stamped BudgetExceeded). The model claimed
// status:ok; the runtime must FORCE status:error while keeping the
// summary, and a worker must NOT abort the mission.
func TestOnChildFrame_BudgetFinalize_ForcesError(t *testing.T) {
	ext := NewExtension(Config{})
	state := newFakeState("mis-1")
	ext.InitState(context.Background(), state) // skipcq
	m := FromState(state)
	m.BeginWave(Wave{Label: "do-1"}) // a worker (Do) wave
	m.RegisterWorker("wrk-1", workerCursor{Name: "analyst", Role: "data-analyst"})

	frame := &protocol.AgentMessage{
		Payload: protocol.AgentMessagePayload{
			Final:          true,
			Consolidated:   true,
			BudgetExceeded: true,
			Text:           "```handoff\n{\"status\":\"ok\",\"body\":{\"got\":\"partial\"},\"memory_summary\":\"did half\"}\n```",
		},
	}
	ext.OnChildFrame(context.Background(), state, "wrk-1", frame)

	got, ok := m.Handoffs.Get("analyst@do-1")
	if !ok {
		t.Fatal("budget-finalize handoff must be recorded")
	}
	if got.Status != "error" {
		t.Fatalf("status = %q, want error (runtime override of the model's ok)", got.Status)
	}
	if !strings.Contains(got.Reason, "context_budget") {
		t.Fatalf("reason = %q, want context_budget", got.Reason)
	}
	if _, aborted := m.BudgetAbortInfo(); aborted {
		t.Fatal("a worker budget-finalize must NOT abort the mission")
	}
}

// TestOnChildFrame_BudgetFinalize_PreservesRawSummary: when the
// budget-cut role wraps up with a FREE-TEXT summary (not a clean fenced
// handoff), the raw text is kept as the handoff body so the planner can
// mission:get_handoff(ref) to see what was accomplished.
func TestOnChildFrame_BudgetFinalize_PreservesRawSummary(t *testing.T) {
	ext := NewExtension(Config{})
	state := newFakeState("mis-1")
	ext.InitState(context.Background(), state) // skipcq
	m := FromState(state)
	m.BeginWave(Wave{Label: "do-1"})
	m.RegisterWorker("wrk-1", workerCursor{Name: "analyst", Role: "data-analyst"})

	frame := &protocol.AgentMessage{
		Payload: protocol.AgentMessagePayload{
			Final: true, Consolidated: true, BudgetExceeded: true,
			Text: "I fetched the geozones and started the spatial join but ran out of budget before aggregating.",
		},
	}
	ext.OnChildFrame(context.Background(), state, "wrk-1", frame)

	got, ok := m.Handoffs.Get("analyst@do-1")
	if !ok {
		t.Fatal("handoff not recorded")
	}
	if got.Status != "error" {
		t.Fatalf("status = %q, want error", got.Status)
	}
	body, _ := got.Body.(string)
	if !strings.Contains(body, "spatial join") {
		t.Fatalf("body = %v, want the worker's raw summary preserved for get_handoff", got.Body)
	}
}

func TestOnChildFrame_BudgetFinalize_OrchestrationAborts(t *testing.T) {
	ext := NewExtension(Config{})
	state := newFakeState("mis-1")
	ext.InitState(context.Background(), state) // skipcq
	m := FromState(state)
	m.BeginWave(Wave{Label: researchWaveLabelPrefix + "1"})
	m.RegisterWorker("rsc-1", workerCursor{Name: "researcher", Role: "researcher"})

	frame := &protocol.AgentMessage{
		Payload: protocol.AgentMessagePayload{
			Final: true, Consolidated: true, BudgetExceeded: true,
			Text: "partial research summary",
		},
	}
	ext.OnChildFrame(context.Background(), state, "rsc-1", frame)

	if role, ok := m.BudgetAbortInfo(); !ok || role != "researcher" {
		t.Fatalf("BudgetAbortInfo = (%q,%v), want (researcher,true)", role, ok)
	}
}

func TestMissionIncompleteReason(t *testing.T) {
	state := newFakeState("mis-1")
	NewExtension(Config{}).InitState(context.Background(), state) // skipcq
	// No abort flag → generic stage-failure reason.
	if r := missionIncompleteReason(state); !strings.Contains(r, "stage failed") {
		t.Fatalf("generic reason = %q, want a stage-failure phrasing", r)
	}
	// Budget abort → names the role + the cause.
	FromState(state).MarkBudgetAbort("researcher")
	if r := missionIncompleteReason(state); !strings.Contains(r, "researcher") || !strings.Contains(r, "context budget") {
		t.Fatalf("budget reason = %q, want researcher + context budget", r)
	}
}

// TestBuildSynthesisTask_Incomplete pins that an abort threads the
// "what happened" reason into the synthesizer's task (the normal-path
// message), while a clean run carries no abort framing.
func TestBuildSynthesisTask_Incomplete(t *testing.T) {
	mission := newRenderedFakeState("mis-synth", productionRenderer(t))

	task, err := buildSynthesisTask(mission, MissionManifest{}, "count roads", "the researcher ran out of its context budget")
	if err != nil {
		t.Fatalf("buildSynthesisTask: %v", err)
	}
	if !strings.Contains(task, "did NOT complete") || !strings.Contains(task, "context budget") {
		t.Fatalf("incomplete task missing the abort framing:\n%s", task)
	}

	clean, err := buildSynthesisTask(mission, MissionManifest{}, "count roads", "")
	if err != nil {
		t.Fatalf("buildSynthesisTask clean: %v", err)
	}
	if strings.Contains(clean, "did NOT complete") {
		t.Fatalf("clean task must not carry the abort framing:\n%s", clean)
	}
}

func TestIsOrchestrationWave(t *testing.T) {
	cases := []struct {
		wave string
		want bool
	}{
		{plannerWaveLabelPrefix + "1", true},
		{checkerWaveLabelPrefix + "2", true},
		{researchWaveLabelPrefix + "1", true},
		{synthesisWaveLabel, true},
		{"do-1", false},
		{"fetch-roads", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isOrchestrationWave(tc.wave); got != tc.want {
			t.Errorf("isOrchestrationWave(%q) = %v, want %v", tc.wave, got, tc.want)
		}
	}
}
