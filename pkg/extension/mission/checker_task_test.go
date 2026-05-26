package mission

import (
	"strings"
	"testing"
)

// TestBuildCheckerTask_RendersMissionACWithStableIDs verifies the
// wiring from state.AC → projectMissionACForTemplate → checkerTaskView
// → checker_task.tmpl produces a task body that:
//
//  1. Contains the [Mission acceptance criteria] header.
//  2. Renders each row with its stable `ac-N` id (so checker can
//     emit `ac_update[]` keyed by id — never invent ids).
//  3. Carries the row's status + last evidence so the checker can
//     decide whether to re-confirm or flip the status.
//
// This is the foundational guarantee that the checker's authority
// over AC status is actually wireable: omit this section and the
// checker has no roster to consult.
func TestBuildCheckerTask_RendersMissionACWithStableIDs(t *testing.T) {
	state := newRenderedFakeState("mis-checker-renders-ac", productionRenderer(t))
	installMissionState(&state.fakeState)
	m := FromState(state)
	if m == nil {
		t.Fatal("FromState=nil")
	}
	// Seed two rows; flip ac-1 satisfied to verify the status marker
	// + evidence land in the rendered output, ac-2 stays unsatisfied.
	m.SeedAC([]ACAddSpec{
		{Statement: "Discovery wave runs"},
		{Statement: "Schema captured"},
	}, OriginManifest)
	if err := m.ApplyStatusOnly([]ACUpdateSpec{
		{ID: "ac-1", Status: ACSatisfied, Evidence: "wrk@x carries 4-table list"},
	}, 1, "checker iter-1"); err != nil {
		t.Fatalf("ApplyStatusOnly: %v", err)
	}

	manifest := MissionManifest{Name: "test", Control: ControlManifest{Role: "checker"}}
	task, err := buildCheckerTask(state, manifest, "user goal text", 2)
	if err != nil {
		t.Fatalf("buildCheckerTask: %v", err)
	}

	for _, want := range []string{
		"[Mission acceptance criteria",
		"`ac-1`",
		"[satisfied]",
		"Discovery wave runs",
		"wrk@x carries 4-table list",
		"`ac-2`",
		"[unsatisfied]",
		"Schema captured",
		"Reference each row by its stable `ac-N` id",
	} {
		if !strings.Contains(task, want) {
			t.Errorf("checker task missing %q. Excerpt:\n%s",
				want, excerpt(task, 4000))
		}
	}
}

// TestBuildCheckerTask_RendersDroppedRowWithReason verifies the drop
// marker + drop_reason both surface in the rendered roster so the
// checker knows the row is out of contract and shouldn't be claimed.
func TestBuildCheckerTask_RendersDroppedRowWithReason(t *testing.T) {
	state := newRenderedFakeState("mis-checker-renders-dropped", productionRenderer(t))
	installMissionState(&state.fakeState)
	m := FromState(state)
	m.SeedAC([]ACAddSpec{
		{Statement: "Keep this row"},
		{Statement: "Drop this row"},
	}, OriginManifest)
	if err := m.StagePlannerDiff(ACDiff{Update: []ACUpdateSpec{
		{ID: "ac-2", Drop: true, DropReason: "user removed from scope"},
	}}, 1, PlannerOriginAt(1), ""); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if _, err := m.CommitStagedDiff(ACDiff{}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	manifest := MissionManifest{Name: "test", Control: ControlManifest{Role: "checker"}}
	task, err := buildCheckerTask(state, manifest, "user goal", 2)
	if err != nil {
		t.Fatalf("buildCheckerTask: %v", err)
	}
	for _, want := range []string{
		"`ac-1`",
		"[unsatisfied]",
		"Keep this row",
		"`ac-2`",
		"[dropped: user removed from scope]",
		"Drop this row",
		"out of contract",
	} {
		if !strings.Contains(task, want) {
			t.Errorf("checker task missing %q. Excerpt:\n%s",
				want, excerpt(task, 4000))
		}
	}
}

// TestBuildCheckerTask_NoACBlockWhenEmpty verifies the conditional
// render — when state.AC is empty, the [Mission acceptance criteria]
// section is omitted entirely (the prose downstream still makes
// sense; the checker just has no roster to consult and the finish
// gate becomes a no-op for that mission).
func TestBuildCheckerTask_NoACBlockWhenEmpty(t *testing.T) {
	state := newRenderedFakeState("mis-checker-no-ac", productionRenderer(t))
	installMissionState(&state.fakeState)
	manifest := MissionManifest{Name: "test", Control: ControlManifest{Role: "checker"}}
	task, err := buildCheckerTask(state, manifest, "user goal", 1)
	if err != nil {
		t.Fatalf("buildCheckerTask: %v", err)
	}
	// "MUST be satisfied for" only appears in the section HEADER,
	// not in the downstream prose's `[Mission acceptance criteria]`
	// back-references. Absence confirms the conditional render
	// skipped the whole block.
	if strings.Contains(task, "MUST be satisfied for `finish`") {
		t.Errorf("empty state.AC should NOT render mission AC block; got:\n%s",
			excerpt(task, 2000))
	}
}

func excerpt(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…(truncated)"
}
