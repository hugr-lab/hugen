package mission

import "testing"

// missionStateForRoadmap builds a mission session state carrying a
// MissionState, the shape collectRoadmap / collectPendingRoadmap
// resolve via FromState. Phase 6.x — the roadmap source is
// PlanState.Roadmap, not a planner handoff.
func missionStateForRoadmap(t *testing.T) (*fakeState, *MissionState) {
	t.Helper()
	mission := newFakeState("mis-roadmap")
	m := NewMissionState()
	mission.SetValue(StateKey, m)
	return mission, m
}

// TestRoadmapReaders_ReadFromPlanState is the regression guard for the
// Phase 6.x fence-removal: the planner no longer emits a ```plan```
// fence, so DecodePlan can't recover its roadmap from the (body-less)
// planner completion marker. The roadmap now lives on
// PlanState.Roadmap (written by validate_and_approve); both readers
// must project it, flagging / filtering entries whose wave already ran.
func TestRoadmapReaders_ReadFromPlanState(t *testing.T) {
	mission, m := missionStateForRoadmap(t)

	m.SetRoadmap([]RoadmapEntry{
		{Label: "w1", Description: "first"},
		{Label: "w2", Description: "second"},
		{Label: "w3", Description: "third"},
	})
	// w1 completed; w2 is the in-flight wave (Active, not yet in Done);
	// w3 is still pending.
	m.Plan.Done = []DoneWave{{Label: "w1", Status: WaveStatusOk}}
	m.Plan.Active = &Wave{Label: "w2"}

	full := collectRoadmap(mission)
	if len(full) != 3 {
		t.Fatalf("collectRoadmap len = %d, want 3 (%+v)", len(full), full)
	}
	wantDone := map[string]bool{"w1": true, "w2": true, "w3": false}
	for _, r := range full {
		if want := wantDone[r.Label]; want != r.Done {
			t.Errorf("collectRoadmap[%s].Done = %v, want %v", r.Label, r.Done, want)
		}
	}

	pending := collectPendingRoadmap(mission)
	if len(pending) != 1 || pending[0].Label != "w3" {
		t.Fatalf("collectPendingRoadmap = %+v, want only w3", pending)
	}
	if pending[0].Description != "third" {
		t.Errorf("pending description = %q, want %q", pending[0].Description, "third")
	}
}

// TestRoadmapReaders_EmptyWhenNoRoadmap: a mission whose plan carried
// no roadmap (or none approved yet) yields nil from both readers — the
// checker's completeness gate sees no pending entries and the planner's
// [Roadmap] section is empty rather than stale.
func TestRoadmapReaders_EmptyWhenNoRoadmap(t *testing.T) {
	mission, _ := missionStateForRoadmap(t)
	if got := collectRoadmap(mission); got != nil {
		t.Errorf("collectRoadmap with no roadmap = %+v, want nil", got)
	}
	if got := collectPendingRoadmap(mission); got != nil {
		t.Errorf("collectPendingRoadmap with no roadmap = %+v, want nil", got)
	}
}

// TestStageAndEmit_PersistsRoadmapOnApprove pins the write half: the
// single validate_and_approve chokepoint copies the approved plan's
// roadmap onto PlanState. A non-approve (refine / abort) carries no
// plan, so it must leave the prior roadmap untouched.
func TestStageAndEmit_PersistsRoadmapOnApprove(t *testing.T) {
	e := newPlannerExtension()
	mission, m := missionStateForRoadmap(t)

	plan := &Plan{Roadmap: []RoadmapEntry{{Label: "ph2", Description: "analyse"}}}
	if _, err := e.stageAndEmit(mission, m, "ses-planner", plan, validateResult{Approved: true}); err != nil {
		t.Fatalf("stageAndEmit approve: %v", err)
	}
	if got := m.Plan.Roadmap; len(got) != 1 || got[0].Label != "ph2" {
		t.Fatalf("after approve, Plan.Roadmap = %+v, want [ph2]", got)
	}

	// A subsequent refine (no plan) must NOT wipe the persisted roadmap.
	if _, err := e.stageAndEmit(mission, m, "ses-planner", nil, validateResult{RefineText: "narrow it"}); err != nil {
		t.Fatalf("stageAndEmit refine: %v", err)
	}
	if got := m.Plan.Roadmap; len(got) != 1 || got[0].Label != "ph2" {
		t.Errorf("refine clobbered the roadmap: Plan.Roadmap = %+v, want [ph2]", got)
	}
}
