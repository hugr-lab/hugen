package mission

import (
	"strings"
	"testing"
)

func TestSeedAC_MintsIDsAndPreservesOrder(t *testing.T) {
	m := NewMissionState()
	items := []ACAddSpec{
		{Statement: "Task row created"},
		{Statement: "Schedule parses"},
		{Statement: "  "}, // whitespace — silently dropped
		{Statement: "Notification fires once"},
	}
	ids := m.SeedAC(items, OriginManifest)
	wantIDs := []string{"ac-1", "ac-2", "", "ac-3"}
	if len(ids) != len(wantIDs) {
		t.Fatalf("ids length=%d, want %d (%v)", len(ids), len(wantIDs), ids)
	}
	for i := range wantIDs {
		if ids[i] != wantIDs[i] {
			t.Errorf("ids[%d]=%q, want %q", i, ids[i], wantIDs[i])
		}
	}
	snap := m.ACSnapshot()
	if len(snap) != 3 {
		t.Fatalf("ACSnapshot()=%d entries, want 3 (whitespace dropped)", len(snap))
	}
	for i, row := range snap {
		if row.Origin != OriginManifest {
			t.Errorf("row %d origin=%q, want %q", i, row.Origin, OriginManifest)
		}
		if row.Status != ACUnsatisfied {
			t.Errorf("row %d default status=%q, want unsatisfied", i, row.Status)
		}
		if row.AddedAtIter != 0 {
			t.Errorf("row %d AddedAtIter=%d, want 0 (manifest seed)", i, row.AddedAtIter)
		}
	}
}

func TestStagePlannerDiff_HoldsBeforeCommit(t *testing.T) {
	m := NewMissionState()
	m.SeedAC([]ACAddSpec{{Statement: "ac-1 seed"}}, OriginManifest)

	diff := ACDiff{
		Add: []ACAddSpec{{Statement: "Include weekly comparison"}},
		Update: []ACUpdateSpec{
			{ID: "ac-1", Statement: "ac-1 reworded"},
		},
	}
	if err := m.StagePlannerDiff(diff, 2, PlannerOriginAt(2), "scope changed"); err != nil {
		t.Fatalf("StagePlannerDiff: %v", err)
	}
	// Before commit: AC should still be the original seed.
	snap := m.ACSnapshot()
	if len(snap) != 1 {
		t.Fatalf("after stage, len(AC)=%d, want 1 (still pre-commit)", len(snap))
	}
	if snap[0].Statement != "ac-1 seed" {
		t.Errorf("statement got modified before commit: %q", snap[0].Statement)
	}
	// And pending diff is retrievable.
	pending := m.PendingDiff()
	if pending == nil {
		t.Fatal("PendingDiff() returned nil after stage")
	}
	if len(pending.Add) != 1 || len(pending.Update) != 1 {
		t.Errorf("pending diff shape: Add=%d Update=%d", len(pending.Add), len(pending.Update))
	}
	if got := m.PendingDiffReason(); got != "scope changed" {
		t.Errorf("PendingDiffReason()=%q, want %q", got, "scope changed")
	}
}

func TestStagePlannerDiff_RejectsInvalid(t *testing.T) {
	m := NewMissionState()
	m.SeedAC([]ACAddSpec{{Statement: "ac-1 seed"}}, OriginManifest)
	// Update referencing an unknown id.
	err := m.StagePlannerDiff(ACDiff{Update: []ACUpdateSpec{
		{ID: "ac-999", Status: ACSatisfied},
	}}, 1, PlannerOriginAt(1), "")
	if err == nil || !strings.Contains(err.Error(), `id "ac-999" does not match`) {
		t.Fatalf("expected unknown-id rejection, got: %v", err)
	}
}

func TestCommitStagedDiff_AppliesAddAndUpdate(t *testing.T) {
	m := NewMissionState()
	m.SeedAC([]ACAddSpec{
		{Statement: "ac-1 seed"},
		{Statement: "ac-2 seed"},
	}, OriginManifest)

	diff := ACDiff{
		Add: []ACAddSpec{{Statement: "Include weekly comparison"}},
		Update: []ACUpdateSpec{
			{ID: "ac-1", Statement: "ac-1 reworded"},
			{ID: "ac-2", Drop: true, DropReason: "out of scope"},
		},
	}
	if err := m.StagePlannerDiff(diff, 2, PlannerOriginAt(2), "user removed payment"); err != nil {
		t.Fatalf("StagePlannerDiff: %v", err)
	}
	added, err := m.CommitStagedDiff(ACDiff{})
	if err != nil {
		t.Fatalf("CommitStagedDiff: %v", err)
	}
	if len(added) != 1 || added[0] != "ac-3" {
		t.Fatalf("added=%v, want [ac-3]", added)
	}
	snap := m.ACSnapshot()
	if len(snap) != 3 {
		t.Fatalf("after commit, len(AC)=%d, want 3", len(snap))
	}
	byID := indexByID(snap)
	if byID["ac-1"].Statement != "ac-1 reworded" {
		t.Errorf("ac-1 statement=%q, want reworded", byID["ac-1"].Statement)
	}
	if byID["ac-2"].Status != ACDropped {
		t.Errorf("ac-2 status=%q, want dropped", byID["ac-2"].Status)
	}
	if byID["ac-2"].DropReason != "out of scope" {
		t.Errorf("ac-2 drop_reason=%q", byID["ac-2"].DropReason)
	}
	if byID["ac-2"].DroppedAtIter != 2 {
		t.Errorf("ac-2 DroppedAtIter=%d, want 2", byID["ac-2"].DroppedAtIter)
	}
	if byID["ac-3"].Origin != PlannerOriginAt(2) {
		t.Errorf("ac-3 origin=%q, want planner_iter_2", byID["ac-3"].Origin)
	}
	if byID["ac-3"].AddedAtIter != 2 {
		t.Errorf("ac-3 AddedAtIter=%d, want 2", byID["ac-3"].AddedAtIter)
	}
	// Staging cleared.
	if m.PendingDiff() != nil {
		t.Error("PendingDiff() not cleared after commit")
	}
}

func TestCommitStagedDiff_RefineExtraTaggedUserRefine(t *testing.T) {
	m := NewMissionState()
	m.SeedAC([]ACAddSpec{{Statement: "ac-1 seed"}}, OriginManifest)
	if err := m.StagePlannerDiff(ACDiff{
		Add: []ACAddSpec{{Statement: "planner-add"}},
	}, 1, PlannerOriginAt(1), ""); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	// User refines: adds one more AC of their own.
	added, err := m.CommitStagedDiff(ACDiff{
		Add: []ACAddSpec{{Statement: "user-refine-add"}},
	})
	if err != nil {
		t.Fatalf("CommitStagedDiff: %v", err)
	}
	if len(added) != 2 {
		t.Fatalf("added=%v, want 2 ids", added)
	}
	snap := indexByID(m.ACSnapshot())
	if snap[added[0]].Origin != PlannerOriginAt(1) {
		t.Errorf("first add origin=%q, want planner_iter_1", snap[added[0]].Origin)
	}
	if snap[added[1]].Origin != OriginUserRefine {
		t.Errorf("second add origin=%q, want user_refine", snap[added[1]].Origin)
	}
}

func TestDiscardStagedDiff_LeavesACUntouched(t *testing.T) {
	m := NewMissionState()
	m.SeedAC([]ACAddSpec{{Statement: "ac-1 seed"}}, OriginManifest)
	if err := m.StagePlannerDiff(ACDiff{
		Update: []ACUpdateSpec{{ID: "ac-1", Statement: "rewrite"}},
	}, 1, PlannerOriginAt(1), ""); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	rejected := m.DiscardStagedDiff()
	if rejected == nil || len(rejected.Update) != 1 {
		t.Fatalf("DiscardStagedDiff() returned %+v, want the rejected diff", rejected)
	}
	if m.PendingDiff() != nil {
		t.Error("PendingDiff() not cleared after discard")
	}
	if got := m.ACSnapshot()[0].Statement; got != "ac-1 seed" {
		t.Errorf("statement got mutated after reject: %q", got)
	}
}

func TestApplyStatusOnly_RejectsContractFields(t *testing.T) {
	m := NewMissionState()
	m.SeedAC([]ACAddSpec{{Statement: "ac-1"}}, OriginManifest)
	err := m.ApplyStatusOnly([]ACUpdateSpec{
		{ID: "ac-1", Statement: "rewrite", Status: ACSatisfied},
	}, 1, "checker iter-1")
	if err == nil || !strings.Contains(err.Error(), "ApplyStatusOnly rejects contract changes") {
		t.Fatalf("expected contract-change rejection, got: %v", err)
	}
}

func TestApplyStatusOnly_AppliesAndStampsEvidence(t *testing.T) {
	m := NewMissionState()
	m.SeedAC([]ACAddSpec{
		{Statement: "ac-1"},
		{Statement: "ac-2"},
	}, OriginManifest)
	err := m.ApplyStatusOnly([]ACUpdateSpec{
		{ID: "ac-1", Status: ACSatisfied, Evidence: "wave-1 handoff"},
		{ID: "ac-2", Status: ACSatisfied}, // no per-row evidence → falls back to source
	}, 3, "checker iter-3")
	if err != nil {
		t.Fatalf("ApplyStatusOnly: %v", err)
	}
	snap := indexByID(m.ACSnapshot())
	if snap["ac-1"].LastEvidence != "wave-1 handoff" {
		t.Errorf("ac-1 evidence=%q", snap["ac-1"].LastEvidence)
	}
	if snap["ac-2"].LastEvidence != "checker iter-3" {
		t.Errorf("ac-2 evidence=%q, want fallback", snap["ac-2"].LastEvidence)
	}
	if snap["ac-1"].SatisfiedAtIter != 3 {
		t.Errorf("ac-1 SatisfiedAtIter=%d, want 3", snap["ac-1"].SatisfiedAtIter)
	}
}

func TestApplyWorkerSatisfies_HappyPath(t *testing.T) {
	m := NewMissionState()
	m.SeedAC([]ACAddSpec{{Statement: "ac-1"}, {Statement: "ac-2"}}, OriginManifest)
	applied, unknown := m.ApplyWorkerSatisfies([]string{"ac-1", "ac-2"}, 2, "data-analyst", "extract")
	if len(applied) != 2 {
		t.Fatalf("applied=%v, want 2 ids", applied)
	}
	if len(unknown) != 0 {
		t.Errorf("unknown=%v, want empty", unknown)
	}
	snap := indexByID(m.ACSnapshot())
	if snap["ac-1"].Status != ACSatisfied {
		t.Errorf("ac-1 status=%q", snap["ac-1"].Status)
	}
	wantEvidence := "worker data-analyst handoff iter-2 wave-extract"
	if snap["ac-1"].LastEvidence != wantEvidence {
		t.Errorf("ac-1 evidence=%q, want %q", snap["ac-1"].LastEvidence, wantEvidence)
	}
	if snap["ac-1"].SatisfiedAtIter != 2 {
		t.Errorf("ac-1 SatisfiedAtIter=%d, want 2", snap["ac-1"].SatisfiedAtIter)
	}
}

func TestApplyWorkerSatisfies_UnknownIDIsBestEffort(t *testing.T) {
	m := NewMissionState()
	m.SeedAC([]ACAddSpec{{Statement: "ac-1"}}, OriginManifest)
	applied, unknown := m.ApplyWorkerSatisfies([]string{"ac-1", "ac-999"}, 1, "x", "y")
	if len(applied) != 1 || applied[0] != "ac-1" {
		t.Errorf("applied=%v", applied)
	}
	if len(unknown) != 1 || unknown[0] != "ac-999" {
		t.Errorf("unknown=%v", unknown)
	}
}

func TestApplyWorkerSatisfies_NoOverrideOnAlreadySatisfied(t *testing.T) {
	m := NewMissionState()
	m.SeedAC([]ACAddSpec{{Statement: "ac-1"}}, OriginManifest)
	_ = m.ApplyStatusOnly([]ACUpdateSpec{
		{ID: "ac-1", Status: ACSatisfied, Evidence: "checker iter-1"},
	}, 1, "checker iter-1")
	applied, _ := m.ApplyWorkerSatisfies([]string{"ac-1"}, 2, "x", "y")
	if len(applied) != 1 {
		t.Errorf("applied=%v", applied)
	}
	snap := indexByID(m.ACSnapshot())
	if snap["ac-1"].LastEvidence != "checker iter-1" {
		t.Errorf("worker overwrote checker evidence: %q", snap["ac-1"].LastEvidence)
	}
	if snap["ac-1"].SatisfiedAtIter != 1 {
		t.Errorf("worker overwrote SatisfiedAtIter: %d", snap["ac-1"].SatisfiedAtIter)
	}
}

func TestHasUnsatisfiedAC(t *testing.T) {
	m := NewMissionState()
	if m.HasUnsatisfiedAC() {
		t.Error("empty AC should not be unsatisfied")
	}
	m.SeedAC([]ACAddSpec{{Statement: "ac-1"}, {Statement: "ac-2"}}, OriginManifest)
	if !m.HasUnsatisfiedAC() {
		t.Error("fresh AC should be unsatisfied")
	}
	_ = m.ApplyStatusOnly([]ACUpdateSpec{
		{ID: "ac-1", Status: ACSatisfied},
		{ID: "ac-2", Status: ACSatisfied},
	}, 1, "checker iter-1")
	if m.HasUnsatisfiedAC() {
		t.Error("all-satisfied AC should not gate")
	}
}

func TestUnsatisfiedAC_OnlyUnsatisfiedRows(t *testing.T) {
	m := NewMissionState()
	m.SeedAC([]ACAddSpec{
		{Statement: "ac-1 satisfied"},
		{Statement: "ac-2 pending"},
		{Statement: "ac-3 dropped"},
	}, OriginManifest)
	if err := m.StagePlannerDiff(ACDiff{Update: []ACUpdateSpec{
		{ID: "ac-3", Drop: true, DropReason: "scope"},
	}}, 1, PlannerOriginAt(1), ""); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if _, err := m.CommitStagedDiff(ACDiff{}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	_ = m.ApplyStatusOnly([]ACUpdateSpec{
		{ID: "ac-1", Status: ACSatisfied},
	}, 2, "checker iter-2")
	pending := m.UnsatisfiedAC()
	if len(pending) != 1 || pending[0].ID != "ac-2" {
		t.Errorf("UnsatisfiedAC()=%v, want [ac-2]", pending)
	}
}

func indexByID(rows []AcceptanceCriterion) map[string]AcceptanceCriterion {
	out := make(map[string]AcceptanceCriterion, len(rows))
	for _, r := range rows {
		out[r.ID] = r
	}
	return out
}
