package mission

import (
	"strings"
	"testing"
)

// TestSpawnInputs_SetGetRoundtrip covers the bare data shape: a
// map passed to SetSpawnInputs is returned by SpawnInputs() with
// the same keys/values, and the returned map is a defensive copy
// (mutating it doesn't affect MissionState).
func TestSpawnInputs_SetGetRoundtrip(t *testing.T) {
	m := NewMissionState()
	m.SetSpawnInputs(map[string]any{
		"file_path":     "~/Downloads/report.html",
		"output_format": "html",
	})
	got := m.SpawnInputs()
	if got["file_path"] != "~/Downloads/report.html" {
		t.Errorf("file_path = %v, want ~/Downloads/report.html", got["file_path"])
	}
	if got["output_format"] != "html" {
		t.Errorf("output_format = %v, want html", got["output_format"])
	}
	// Defensive copy — mutating `got` must not leak into state.
	got["file_path"] = "MUTATED"
	if again := m.SpawnInputs(); again["file_path"] != "~/Downloads/report.html" {
		t.Errorf("state mutated through returned map: %v", again["file_path"])
	}
}

// TestSpawnInputs_NilAndNonMapBehaviour covers the contract: nil
// or non-map values normalise to "no inputs" (SpawnInputs returns
// nil). Empty maps also normalise to nil so callers can do a
// single `if len(m.SpawnInputs()) == 0` check.
func TestSpawnInputs_NilAndNonMapBehaviour(t *testing.T) {
	m := NewMissionState()

	// Nil any → nil out.
	m.SetSpawnInputs(nil)
	if got := m.SpawnInputs(); got != nil {
		t.Errorf("SpawnInputs after Set(nil) = %v, want nil", got)
	}

	// Non-map (string) → nil out (caller misuse, fail closed).
	m.SetSpawnInputs("just a string")
	if got := m.SpawnInputs(); got != nil {
		t.Errorf("SpawnInputs after Set(string) = %v, want nil", got)
	}

	// Empty map → nil out.
	m.SetSpawnInputs(map[string]any{})
	if got := m.SpawnInputs(); got != nil {
		t.Errorf("SpawnInputs after Set(empty map) = %v, want nil", got)
	}

	// Re-set with non-empty map clears the empty-state and stamps.
	m.SetSpawnInputs(map[string]any{"k": "v"})
	if got := m.SpawnInputs(); got["k"] != "v" {
		t.Errorf("SpawnInputs after re-Set = %+v, want {k: v}", got)
	}
}

// TestBuildPlannerTask_RendersSpawnInputs is the load-bearing
// template assertion: spawn-time inputs must surface in the
// planner's task brief under the `[Inputs from caller — propagate
// VERBATIM to worker inputs]` section. This is what closes the
// bug where the planner invented a filename instead of using the
// caller's `file_path`.
func TestBuildPlannerTask_RendersSpawnInputs(t *testing.T) {
	state := newRenderedFakeState("mis-spawn-inputs-planner", productionRenderer(t))
	installMissionState(&state.fakeState)
	m := FromState(state)
	if m == nil {
		t.Fatal("FromState=nil")
	}
	m.SetSpawnInputs(map[string]any{
		"file_path":     "~/Downloads/op2023_report.html",
		"output_format": "html",
	})

	manifest := MissionManifest{
		Name: "spawn-inputs-test",
		Plan: MissionPlanManifest{
			Role:     "planner",
			Approval: PlanApproval{Initial: ApprovalInitialSkip},
			MaxWaves: 5,
		},
	}
	task, err := buildPlannerTask(state, manifest, "Generate report from op2023", 1, nil)
	if err != nil {
		t.Fatalf("buildPlannerTask: %v", err)
	}
	for _, want := range []string{
		"[Inputs from caller",
		"propagate VERBATIM",
		"file_path: ~/Downloads/op2023_report.html",
		"output_format: html",
		"NEVER invent a substitute",
	} {
		if !strings.Contains(task, want) {
			t.Errorf("planner task missing %q. Excerpt:\n%s", want, excerpt(task, 6000))
		}
	}
}

// TestBuildPlannerTask_OmitsSpawnInputsBlockWhenEmpty verifies the
// guard: missions spawned without inputs (legacy callers, ad-hoc
// chat-driven missions) must NOT render an empty `[Inputs from
// caller]` section — that would just be visual noise the planner
// would have to skip past.
func TestBuildPlannerTask_OmitsSpawnInputsBlockWhenEmpty(t *testing.T) {
	state := newRenderedFakeState("mis-no-spawn-inputs-planner", productionRenderer(t))
	installMissionState(&state.fakeState)
	// Do not call SetSpawnInputs — default zero state.

	manifest := MissionManifest{
		Name: "no-inputs",
		Plan: MissionPlanManifest{
			Role:     "planner",
			Approval: PlanApproval{Initial: ApprovalInitialSkip},
			MaxWaves: 3,
		},
	}
	task, err := buildPlannerTask(state, manifest, "do work", 1, nil)
	if err != nil {
		t.Fatalf("buildPlannerTask: %v", err)
	}
	if strings.Contains(task, "[Inputs from caller") {
		t.Errorf("empty spawnInputs must NOT render the [Inputs from caller] section; got:\n%s",
			excerpt(task, 3000))
	}
}

// TestBuildResearchTask_RendersSpawnInputs verifies the
// researcher's task brief also surfaces spawn-time inputs under
// `[Inputs from caller — already resolved, do NOT re-ask]`. Without
// this the researcher would emit clarifications for keys the
// caller already passed (file_path, output_format, …) — a UX
// regression the user notices as redundant modals.
func TestBuildResearchTask_RendersSpawnInputs(t *testing.T) {
	state := newRenderedFakeState("mis-spawn-inputs-researcher", productionRenderer(t))
	installMissionState(&state.fakeState)
	m := FromState(state)
	m.SetSpawnInputs(map[string]any{
		"file_path":     "~/Downloads/op2023_report.html",
		"output_format": "html",
	})

	manifest := MissionManifest{
		Name:     "spawn-inputs-research",
		Research: &ResearchManifest{Role: "researcher"},
	}
	task, err := buildResearchTask(state, manifest, "do research", nil)
	if err != nil {
		t.Fatalf("buildResearchTask: %v", err)
	}
	for _, want := range []string{
		"[Inputs from caller",
		"already resolved, do NOT re-ask",
		"file_path: ~/Downloads/op2023_report.html",
		"output_format: html",
	} {
		if !strings.Contains(task, want) {
			t.Errorf("research task missing %q. Excerpt:\n%s", want, excerpt(task, 4000))
		}
	}
}

// TestBuildResearchTask_OmitsSpawnInputsBlockWhenEmpty verifies
// the guard for the researcher template: no inputs → no section.
func TestBuildResearchTask_OmitsSpawnInputsBlockWhenEmpty(t *testing.T) {
	state := newRenderedFakeState("mis-no-spawn-inputs-researcher", productionRenderer(t))
	installMissionState(&state.fakeState)
	// SetSpawnInputs never called.

	manifest := MissionManifest{
		Name:     "no-spawn-inputs",
		Research: &ResearchManifest{Role: "researcher"},
	}
	task, err := buildResearchTask(state, manifest, "go", nil)
	if err != nil {
		t.Fatalf("buildResearchTask: %v", err)
	}
	if strings.Contains(task, "[Inputs from caller") {
		t.Errorf("empty spawnInputs must NOT render the [Inputs from caller] section; got:\n%s",
			excerpt(task, 3000))
	}
}
