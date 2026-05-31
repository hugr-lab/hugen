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
