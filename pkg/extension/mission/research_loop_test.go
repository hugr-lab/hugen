package mission

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestRunResearchStage_HappyPath — the researcher runs ONCE and
// emits a terminal kind=research handoff with findings. State
// carries the findings + resolved inputs, MarkResearchAttempted is
// set, and the RUNTIME opened no modal of its own (the researcher
// owns its HITL via session:inquire in-turn — never the runtime).
func TestRunResearchStage_HappyPath(t *testing.T) {
	state := newRenderedFakeState("mis-r-happy", productionRenderer(t))
	installMissionState(&state.fakeState)

	manifest := MissionManifest{
		Name:     "research-mission",
		Research: &ResearchManifest{Role: "researcher"},
	}

	spawner := &plannerFakeSpawner{state: state}
	var spawns atomic.Int32
	spawner.onWorkerSpawn = func(_ SpawnRequest) Handoff {
		spawns.Add(1)
		return Handoff{
			Kind:   KindResearch,
			Status: "ok",
			Body: map[string]any{
				"findings":       "user wants HTML report for the op2023 source; join key contract_id confirmed",
				"memory_summary": "scoping complete",
				"resolved_user_inputs": map[string]any{
					"data_source": "op2023",
					"format":      "html",
				},
			},
		}
	}

	ext := newPlannerExtension()
	executor := NewExecutor(spawner.spawn, ext.logger)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	aborted, err := ext.runResearchStage(ctx, executor, state, manifest, manifest.Name, "build an HTML report")
	if err != nil {
		t.Fatalf("runResearchStage: %v", err)
	}
	if aborted {
		t.Fatalf("aborted = true, want false")
	}
	if got := spawns.Load(); got != 1 {
		t.Errorf("research spawns = %d, want 1 (single pass, no re-fire loop)", got)
	}
	m := FromState(state)
	if !m.ResearchAttempted() {
		t.Error("ResearchAttempted = false, want true after the stage ran")
	}
	findings, resolved, _ := m.ResearchOutput()
	if !strings.Contains(findings, "op2023") {
		t.Errorf("findings = %q, want substring 'op2023'", findings)
	}
	if got := resolved["data_source"]; got != "op2023" {
		t.Errorf("resolved_user_inputs[data_source] = %v, want 'op2023'", got)
	}
	if got := len(state.inquiryRequests); got != 0 {
		t.Errorf("inquiryRequests = %d, want 0 (the runtime never inquires; the researcher does)", got)
	}
}

// TestRunResearchStage_FeasibilityError — the researcher reports
// the mission is not feasible via status:error. The stage aborts
// and records no findings.
func TestRunResearchStage_FeasibilityError(t *testing.T) {
	state := newRenderedFakeState("mis-r-infeasible", productionRenderer(t))
	installMissionState(&state.fakeState)

	manifest := MissionManifest{
		Name:     "research-mission",
		Research: &ResearchManifest{Role: "researcher"},
	}

	spawner := &plannerFakeSpawner{state: state}
	spawner.onWorkerSpawn = func(_ SpawnRequest) Handoff {
		return Handoff{
			Kind:   KindResearch,
			Status: "error",
			Reason: "no table carries road length in any source",
		}
	}

	ext := newPlannerExtension()
	executor := NewExecutor(spawner.spawn, ext.logger)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	aborted, err := ext.runResearchStage(ctx, executor, state, manifest, manifest.Name, "count road length")
	if !aborted {
		t.Errorf("aborted = false, want true on status:error")
	}
	if err == nil {
		t.Fatal("err = nil, want a status-error abort")
	}
	if got, _, _ := FromState(state).ResearchOutput(); got != "" {
		t.Errorf("findings = %q, want empty after abort", got)
	}
}

// TestRunResearchStage_WrongKindRetry — a wrong-kind handoff gets
// the shape-glitch retry budget. After researchValidationRetryCap+1
// failed attempts the stage aborts. Verifies the retry budget caps
// a chronically-broken role instead of looping forever.
func TestRunResearchStage_WrongKindRetry(t *testing.T) {
	state := newRenderedFakeState("mis-r-retry", productionRenderer(t))
	installMissionState(&state.fakeState)

	manifest := MissionManifest{
		Name:     "research-mission",
		Research: &ResearchManifest{Role: "researcher"},
	}

	spawner := &plannerFakeSpawner{state: state}
	var n atomic.Int32
	spawner.onWorkerSpawn = func(_ SpawnRequest) Handoff {
		// Always emit kind=handoff (wrong kind) — never recovers.
		n.Add(1)
		return Handoff{
			Kind:   KindHandoff,
			Status: "ok",
			Body:   "not a research fence",
		}
	}

	ext := newPlannerExtension()
	executor := NewExecutor(spawner.spawn, ext.logger)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	aborted, err := ext.runResearchStage(ctx, executor, state, manifest, manifest.Name, "research that never produces research")
	if !aborted {
		t.Errorf("aborted = false, want true")
	}
	if err == nil || !strings.Contains(err.Error(), "after") {
		t.Errorf("err = %v, want substring 'after N attempts'", err)
	}
	// Budget is researchValidationRetryCap (=2) retries + the first
	// attempt = 3 total spawns before the stage gives up.
	if got, want := n.Load(), int32(researchValidationRetryCap+1); got != want {
		t.Errorf("research spawns = %d, want %d (retry budget reached)", got, want)
	}
}
