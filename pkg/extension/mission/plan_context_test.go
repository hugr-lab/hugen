package mission

import (
	"strings"
	"testing"
	"time"
)

func TestPlanContext_AppendAndList(t *testing.T) {
	pc := NewPlanContext()
	pc.Append(PlanContextEntry{Iteration: 1, Phase: "plan", Summary: "first"})
	pc.Append(PlanContextEntry{Iteration: 1, Phase: "do", Summary: "second"})
	pc.Append(PlanContextEntry{Iteration: 2, Phase: "verdict", Summary: "third"})

	rows := pc.List()
	if len(rows) != 3 {
		t.Fatalf("List len = %d, want 3", len(rows))
	}
	for i, want := range []string{"first", "second", "third"} {
		if rows[i].Summary != want {
			t.Errorf("row %d Summary = %q, want %q", i, rows[i].Summary, want)
		}
	}
	if rows[2].Phase != "verdict" {
		t.Errorf("row 2 Phase = %q, want verdict", rows[2].Phase)
	}
	if rows[0].CreatedAt.IsZero() {
		t.Error("CreatedAt was not auto-filled")
	}
}

func TestPlanContext_AppendHandoff_SkipsEmptySummary(t *testing.T) {
	pc := NewPlanContext()
	// Handoff with no memory_summary — should be a no-op.
	pc.AppendHandoff(1, "wave-1", Handoff{Kind: KindHandoff, Status: "ok", MemorySummary: ""})
	if pc.Len() != 0 {
		t.Errorf("Len = %d, want 0 after empty-summary append", pc.Len())
	}
	pc.AppendHandoff(1, "wave-1", Handoff{
		Kind:          KindHandoff,
		Status:        "ok",
		MemorySummary: "found orders table",
		Subagent:      SubagentRef{Name: "explorer", Role: "schema-explorer"},
	})
	if pc.Len() != 1 {
		t.Fatalf("Len = %d, want 1 after one summary append", pc.Len())
	}
	row := pc.List()[0]
	if row.Phase != "do" {
		t.Errorf("Phase = %q, want do for kind=handoff", row.Phase)
	}
	if row.Role != "schema-explorer" {
		t.Errorf("Role = %q, want schema-explorer", row.Role)
	}
	if row.Wave != "wave-1" {
		t.Errorf("Wave = %q, want wave-1", row.Wave)
	}
}

func TestPlanContext_PhaseInferredFromKind(t *testing.T) {
	cases := []struct {
		kind OutputContractKind
		wave string
		want string
	}{
		{KindHandoff, "wave-1", "do"},
		{KindHandoff, "_plan-1", "plan"},
		{KindHandoff, "_check-1", "verdict"},
		{KindHandoff, "_synthesis", "synthesis"},
		{KindPlan, "_plan-1", "plan"},
		{KindVerdict, "_check-1", "verdict"},
		{KindSynthesis, "_synthesis", "synthesis"},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind)+"/"+tc.wave, func(t *testing.T) {
			got := phaseForHandoff(Handoff{Kind: tc.kind, MemorySummary: "x"}, tc.wave)
			if got != tc.want {
				t.Errorf("phaseForHandoff(%q, %q) = %q, want %q", tc.kind, tc.wave, got, tc.want)
			}
		})
	}
}

func TestPlanContext_FIFOTrimUnderSoftCap(t *testing.T) {
	pc := NewPlanContext()
	// 10 entries of 1000 chars each = 10000 chars → trim should
	// kick in (soft cap is 8000).
	big := strings.Repeat("x", 1000)
	for i := 0; i < 10; i++ {
		pc.Append(PlanContextEntry{
			Iteration: i + 1,
			Phase:     "do",
			Summary:   big,
			CreatedAt: time.Now(),
		})
	}
	rows := pc.List()
	if len(rows) >= 10 {
		t.Fatalf("FIFO trim did not fire — len=%d", len(rows))
	}
	// Last entry preserved.
	if rows[len(rows)-1].Iteration != 10 {
		t.Errorf("newest row Iteration = %d, want 10 (trim ate the wrong end)", rows[len(rows)-1].Iteration)
	}
	// Total char count under the soft cap (or single huge entry).
	total := 0
	for _, r := range rows {
		total += len(r.Summary)
	}
	if total > planContextSoftCap && len(rows) > 1 {
		t.Errorf("post-trim total = %d, want ≤ %d when more than one entry remains", total, planContextSoftCap)
	}
}

func TestPlanContext_AppendHandoff_StampsIngestPathIteration(t *testing.T) {
	// Sanity check: AppendHandoff carries the iteration argument
	// through. Used by ingestHandoff's IterationCounter read.
	pc := NewPlanContext()
	pc.AppendHandoff(7, "_check-7", Handoff{
		Kind: KindVerdict, Status: "ok", MemorySummary: "amend issued",
	})
	rows := pc.List()
	if len(rows) != 1 {
		t.Fatalf("Len = %d, want 1", len(rows))
	}
	if rows[0].Iteration != 7 {
		t.Errorf("Iteration = %d, want 7", rows[0].Iteration)
	}
	if rows[0].Phase != "verdict" {
		t.Errorf("Phase = %q, want verdict", rows[0].Phase)
	}
}
