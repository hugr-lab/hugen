package mission

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	wsext "github.com/hugr-lab/hugen/pkg/extension/workspace"
)

// TestWriteSpecContract_ProjectsGoalAndAC verifies the runtime
// projects the committed goal + AC roster into <mission_dir>/spec.md
// at the validate_and_approve commit chokepoint. Phase 6.x.
func TestWriteSpecContract_ProjectsGoalAndAC(t *testing.T) {
	root := t.TempDir()
	state := newRenderedFakeState("mis-spec", productionRenderer(t))
	if err := wsext.NewExtension(root, nil).InitState(context.Background(), state); err != nil {
		t.Fatalf("workspace InitState: %v", err)
	}
	m := installMissionState(&state.fakeState)
	m.SeedAC([]ACAddSpec{
		{Statement: "Report covers all 2023 contracts"},
		{Statement: "Totals reconcile within 1%"},
	}, OriginManifest)

	ext := newPlannerExtension()
	ext.writeSpecContract(state, m, &Plan{MissionGoal: "Build the 2023 HTML report"})

	dir := wsext.FromState(state).Dir()
	data, err := os.ReadFile(filepath.Join(dir, "spec.md"))
	if err != nil {
		t.Fatalf("read spec.md: %v", err)
	}
	s := string(data)
	if !strings.Contains(s, "Build the 2023 HTML report") {
		t.Errorf("spec.md missing goal:\n%s", s)
	}
	if !strings.Contains(s, "ac-1") || !strings.Contains(s, "ac-2") {
		t.Errorf("spec.md missing AC ids:\n%s", s)
	}
	if !strings.Contains(s, "Totals reconcile within 1%") {
		t.Errorf("spec.md missing AC statement:\n%s", s)
	}
}

// TestWriteSpecContract_GoalFallback verifies a plan_complete approve
// (nil plan) falls back to the prior committed goal rather than
// writing an empty goal. Phase 6.x.
func TestWriteSpecContract_GoalFallback(t *testing.T) {
	root := t.TempDir()
	state := newRenderedFakeState("mis-spec-fallback", productionRenderer(t))
	if err := wsext.NewExtension(root, nil).InitState(context.Background(), state); err != nil {
		t.Fatalf("workspace InitState: %v", err)
	}
	m := installMissionState(&state.fakeState)
	m.SetGoalAndWaveAC("Prior approved goal", nil)

	ext := newPlannerExtension()
	ext.writeSpecContract(state, m, nil) // plan_complete

	data, err := os.ReadFile(filepath.Join(wsext.FromState(state).Dir(), "spec.md"))
	if err != nil {
		t.Fatalf("read spec.md: %v", err)
	}
	if !strings.Contains(string(data), "Prior approved goal") {
		t.Errorf("spec.md missing fallback goal:\n%s", data)
	}
}

// TestWriteSpecContract_NoWorkspace_NoPanic verifies the writer is a
// clean no-op when the session has no workspace dir (test fixtures /
// sessions without the workspace ext). Phase 6.x.
func TestWriteSpecContract_NoWorkspace_NoPanic(t *testing.T) {
	state := newRenderedFakeState("mis-spec-no-ws", productionRenderer(t))
	m := installMissionState(&state.fakeState)
	ext := newPlannerExtension()
	// No workspace wired — wsext.FromState returns nil; must not panic.
	ext.writeSpecContract(state, m, &Plan{MissionGoal: "x"})
}

// TestWriteSpecContract_CheckerStatusReflectedOnDisk pins the B39
// checker-stage write: after the checker satisfies an AC by id, the
// on-disk spec.md flips that row's checkbox to `- [x]` and carries the
// verification evidence — while an unverified row stays `- [ ]`.
func TestWriteSpecContract_CheckerStatusReflectedOnDisk(t *testing.T) {
	root := t.TempDir()
	state := newRenderedFakeState("mis-spec-checker", productionRenderer(t))
	if err := wsext.NewExtension(root, nil).InitState(context.Background(), state); err != nil {
		t.Fatalf("workspace InitState: %v", err)
	}
	m := installMissionState(&state.fakeState)
	m.SeedAC([]ACAddSpec{
		{Statement: "Totals reconcile within 1%"},
		{Statement: "Report covers all 2023 contracts"},
	}, OriginManifest)

	// Checker verifies ac-1 only (status-only update by id).
	if err := m.ApplyStatusOnly([]ACUpdateSpec{
		{ID: "ac-1", Status: ACSatisfied, Evidence: "checker iter-1: totals match within 0.3%"},
	}, 1, "checker iter-1"); err != nil {
		t.Fatalf("ApplyStatusOnly: %v", err)
	}

	ext := newPlannerExtension()
	ext.writeSpecContract(state, m, nil)

	s := readSpecMD(t, state)
	if !strings.Contains(s, "- [x] `ac-1`") {
		t.Errorf("verified ac-1 not checked on disk:\n%s", s)
	}
	if !strings.Contains(s, "totals match within 0.3%") {
		t.Errorf("checker evidence missing from spec.md:\n%s", s)
	}
	if !strings.Contains(s, "- [ ] `ac-2`") {
		t.Errorf("unverified ac-2 should stay unchecked:\n%s", s)
	}
}

// TestWriteSpecContract_ProgressProjectsWaves pins the B39 Progress
// block: completed worker waves (with refs) + the active wave + the
// roadmap project from live PlanState, while runtime orchestration
// waves (_check-* / _plan-* / _synthesis) are filtered out of the
// human-facing snapshot.
func TestWriteSpecContract_ProgressProjectsWaves(t *testing.T) {
	root := t.TempDir()
	state := newRenderedFakeState("mis-spec-progress", productionRenderer(t))
	if err := wsext.NewExtension(root, nil).InitState(context.Background(), state); err != nil {
		t.Fatalf("workspace InitState: %v", err)
	}
	m := installMissionState(&state.fakeState)
	m.SeedAC([]ACAddSpec{{Statement: "x"}}, OriginManifest)

	m.Plan.Done = []DoneWave{
		{Label: "_check-1", Status: WaveStatusOk},                                            // orchestration → filtered
		{Label: "build-report", Status: WaveStatusOk, Refs: []string{"report@build-report"}}, // worker → shown
	}
	m.Plan.Active = &Wave{Label: "validate", Subagents: []SubagentSpec{{Name: "v1"}, {Name: "v2"}}}
	m.Plan.Roadmap = []RoadmapEntry{{Label: "polish", Description: "final formatting pass"}}

	ext := newPlannerExtension()
	ext.writeSpecContract(state, m, &Plan{MissionGoal: "Build the 2023 HTML report"})

	s := readSpecMD(t, state)
	for _, want := range []string{
		"### Completed waves", "build-report", "report@build-report",
		"### Active wave", "validate", "2 worker",
		"### Roadmap", "polish", "final formatting pass",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("progress snapshot missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "_check-1") {
		t.Errorf("orchestration wave leaked into the human snapshot:\n%s", s)
	}
}

// readSpecMD reads <mission_dir>/spec.md for the given session.
func readSpecMD(t *testing.T, state *renderedFakeState) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(wsext.FromState(state).Dir(), "spec.md"))
	if err != nil {
		t.Fatalf("read spec.md: %v", err)
	}
	return string(data)
}
