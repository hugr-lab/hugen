package mission

import (
	"strings"
	"testing"
)

// TestProjectACDiffView_NilStateNilDiffReturnsNil covers the
// defensive branch: no state, no plan diff → caller can drop the
// structured section and render the legacy bullet list.
func TestProjectACDiffView_NilStateNilDiffReturnsNil(t *testing.T) {
	got := projectACDiffViewForApproval(nil, Plan{})
	if got != nil {
		t.Errorf("want nil view, got %+v", got)
	}
}

// TestProjectACDiffView_FirstIterAdd covers iter 1: no committed
// rows yet, planner emits ac_add → view carries only NEW entries.
func TestProjectACDiffView_FirstIterAdd(t *testing.T) {
	mission := newRenderedFakeState("mis-diff-first-iter", productionRenderer(t))
	installMissionState(&mission.fakeState)
	plan := Plan{
		ACAdd: []ACAddSpec{
			{Statement: "Discovery wave runs"},
			{Statement: "Schema captured"},
		},
	}
	view := projectACDiffViewForApproval(mission, plan)
	if len(view) != 2 {
		t.Fatalf("len(view)=%d, want 2", len(view))
	}
	for i, e := range view {
		if e.Change != ACChangeNew {
			t.Errorf("entry %d Change=%q, want new", i, e.Change)
		}
		if e.Status != ACUnsatisfied {
			t.Errorf("entry %d Status=%q, want unsatisfied", i, e.Status)
		}
		if e.ID != "" {
			t.Errorf("entry %d ID=%q, want empty (id minted on commit)", i, e.ID)
		}
	}
	if view[0].Statement != "Discovery wave runs" {
		t.Errorf("view[0]=%q", view[0].Statement)
	}
}

// TestProjectACDiffView_AllChannels exercises all four change tags
// at once: carry / edited / new / dropped. Tests the order of
// emission (carry → edited → new → dropped).
func TestProjectACDiffView_AllChannels(t *testing.T) {
	mission := newRenderedFakeState("mis-diff-all-channels", productionRenderer(t))
	installMissionState(&mission.fakeState)
	m := FromState(mission)
	m.SeedAC([]ACAddSpec{
		{Statement: "carry-1"},
		{Statement: "edited-2"},
		{Statement: "dropped-3"},
	}, OriginManifest)

	plan := Plan{
		ACAdd: []ACAddSpec{{Statement: "new-4"}},
		ACUpdate: []ACUpdateSpec{
			{ID: "ac-2", Statement: "edited-2 reworded"},
			{ID: "ac-3", Drop: true, DropReason: "out of scope"},
		},
	}
	view := projectACDiffViewForApproval(mission, plan)
	if len(view) != 4 {
		t.Fatalf("len(view)=%d, want 4: %+v", len(view), view)
	}
	// Expected order: carry (ac-1), edited (ac-2), new (no id),
	// dropped (ac-3).
	if view[0].Change != ACChangeCarry || view[0].ID != "ac-1" {
		t.Errorf("view[0]=%+v, want carry/ac-1", view[0])
	}
	if view[1].Change != ACChangeEdited || view[1].ID != "ac-2" {
		t.Errorf("view[1]=%+v, want edited/ac-2", view[1])
	}
	if view[1].Statement != "edited-2 reworded" {
		t.Errorf("edited statement not applied: %q", view[1].Statement)
	}
	if view[2].Change != ACChangeNew || view[2].ID != "" || view[2].Statement != "new-4" {
		t.Errorf("view[2]=%+v, want new/empty-id/new-4", view[2])
	}
	if view[3].Change != ACChangeDropped || view[3].ID != "ac-3" {
		t.Errorf("view[3]=%+v, want dropped/ac-3", view[3])
	}
	if view[3].DropReason != "out of scope" {
		t.Errorf("DropReason=%q", view[3].DropReason)
	}
}

// TestProjectACDiffView_StatusOnlyUpdateStaysCarry verifies a
// status-only update doesn't get an [EDITED] tag — it's not a
// contract change.
func TestProjectACDiffView_StatusOnlyUpdateStaysCarry(t *testing.T) {
	mission := newRenderedFakeState("mis-diff-status-only", productionRenderer(t))
	installMissionState(&mission.fakeState)
	m := FromState(mission)
	m.SeedAC([]ACAddSpec{{Statement: "ac-1"}}, OriginManifest)
	plan := Plan{
		ACUpdate: []ACUpdateSpec{{ID: "ac-1", Status: ACSatisfied}},
	}
	view := projectACDiffViewForApproval(mission, plan)
	if len(view) != 1 {
		t.Fatalf("len(view)=%d, want 1", len(view))
	}
	if view[0].Change != ACChangeCarry {
		t.Errorf("status-only should keep Change=carry, got %q", view[0].Change)
	}
	if view[0].Status != ACSatisfied {
		t.Errorf("status overlay not applied: %q", view[0].Status)
	}
}

// TestRenderApprovalQuestion_StructuredDiffIcons covers the
// template render path: the question text carries the per-row
// icons + [NEW]/[EDITED]/[DROPPED] markers.
func TestRenderApprovalQuestion_StructuredDiffIcons(t *testing.T) {
	mission := newRenderedFakeState("mis-render-diff-icons", productionRenderer(t))
	installMissionState(&mission.fakeState)
	m := FromState(mission)
	m.SeedAC([]ACAddSpec{
		{Statement: "carry row"},
		{Statement: "rewrite me"},
	}, OriginManifest)
	plan := Plan{
		MissionGoal: "demo",
		ACAdd:       []ACAddSpec{{Statement: "fresh row"}},
		ACUpdate:    []ACUpdateSpec{{ID: "ac-2", Statement: "rewrote it"}},
		NextWave: Wave{
			Label:     "wave-1",
			Subagents: []SubagentSpec{{Name: "w", Role: "r", Task: "t"}},
		},
		Roadmap:   []RoadmapEntry{{Label: "next", Description: "later"}},
		Rationale: "first iter",
	}
	out, err := renderApprovalQuestion(mission, plan)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{
		"ac-1",
		"carry row",
		"ac-2",
		"rewrote it",
		"[EDITED]",
		"fresh row",
		"[NEW]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered question missing %q. Output:\n%s", want, out)
		}
	}
}

// TestRenderApprovalQuestion_DroppedRowRendersWithReason covers
// the visual feedback for drops: the row appears with ✗ icon +
// [DROPPED: <reason>] suffix so the user understands why it's out.
func TestRenderApprovalQuestion_DroppedRowRendersWithReason(t *testing.T) {
	mission := newRenderedFakeState("mis-render-dropped", productionRenderer(t))
	installMissionState(&mission.fakeState)
	m := FromState(mission)
	m.SeedAC([]ACAddSpec{{Statement: "send via email"}}, OriginManifest)
	plan := Plan{
		MissionGoal: "demo",
		ACUpdate:    []ACUpdateSpec{{ID: "ac-1", Drop: true, DropReason: "out of scope"}},
		NextWave: Wave{
			Label:     "wave-1",
			Subagents: []SubagentSpec{{Name: "w", Role: "r", Task: "t"}},
		},
		Roadmap:   []RoadmapEntry{{Label: "later", Description: "later"}},
		Rationale: "drop the email AC",
	}
	out, err := renderApprovalQuestion(mission, plan)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(out, "[DROPPED: out of scope]") {
		t.Errorf("expected drop marker in output:\n%s", out)
	}
}
