package mission

import (
	"strings"
	"testing"
)

// TestParseHandoff_Satisfies covers the wire-level parse: workers
// emit a `satisfies: ["ac-1", "ac-3"]` field next to body /
// memory_summary, parser carries it into Handoff.Satisfies.
func TestParseHandoff_Satisfies(t *testing.T) {
	raw := "```handoff\n" +
		`{"status":"ok","body":"output","memory_summary":"did the thing","satisfies":["ac-1","ac-3"]}` +
		"\n```"
	h, err := ParseHandoff(raw)
	if err != nil {
		t.Fatalf("ParseHandoff: %v", err)
	}
	if len(h.Satisfies) != 2 {
		t.Fatalf("Satisfies len=%d, want 2 (%v)", len(h.Satisfies), h.Satisfies)
	}
	if h.Satisfies[0] != "ac-1" || h.Satisfies[1] != "ac-3" {
		t.Errorf("Satisfies=%v, want [ac-1 ac-3]", h.Satisfies)
	}
}

// TestParseHandoff_SatisfiesDropsWhitespace verifies the parser
// trims and drops empty entries instead of letting them through
// (otherwise `state.findIDLocked("")` would silently no-op).
func TestParseHandoff_SatisfiesDropsWhitespace(t *testing.T) {
	raw := "```handoff\n" +
		`{"status":"ok","body":"x","satisfies":["ac-1","   ","","ac-2"]}` +
		"\n```"
	h, err := ParseHandoff(raw)
	if err != nil {
		t.Fatalf("ParseHandoff: %v", err)
	}
	if len(h.Satisfies) != 2 {
		t.Fatalf("Satisfies=%v, want 2 entries", h.Satisfies)
	}
	if h.Satisfies[0] != "ac-1" || h.Satisfies[1] != "ac-2" {
		t.Errorf("Satisfies=%v", h.Satisfies)
	}
}

// TestParseHandoff_SatisfiesNonStringIgnored verifies the parser is
// lenient: non-string entries are silently dropped (so a weak model
// emitting `[1, "ac-1"]` doesn't blow up the parse).
func TestParseHandoff_SatisfiesNonStringIgnored(t *testing.T) {
	raw := "```handoff\n" +
		`{"status":"ok","body":"x","satisfies":[1,"ac-1",true,"ac-2"]}` +
		"\n```"
	h, err := ParseHandoff(raw)
	if err != nil {
		t.Fatalf("ParseHandoff: %v", err)
	}
	if len(h.Satisfies) != 2 {
		t.Errorf("Satisfies=%v, want [ac-1 ac-2]", h.Satisfies)
	}
}

// TestIngestHandoff_SatisfiesAppliedToACState covers the
// end-to-end path: a worker handoff body carries satisfies → after
// ingestHandoff, state.AC marks the rows satisfied with the
// canonical evidence string.
func TestIngestHandoff_SatisfiesAppliedToACState(t *testing.T) {
	ext := newPlannerExtension()
	state := newRenderedFakeState("mis-satisfies-ingest", productionRenderer(t))
	installMissionState(&state.fakeState)
	m := FromState(state)
	if m == nil {
		t.Fatal("FromState=nil")
	}
	m.SeedAC([]ACAddSpec{{Statement: "ac-1"}, {Statement: "ac-2"}, {Statement: "ac-3"}}, OriginManifest)
	m.IterationCounter = 2
	m.BeginWave("extract")
	cur := workerCursor{Name: "wkr", Role: "data-analyst", Skill: "analyst"}
	m.RegisterWorker("child-1", cur)

	text := "```handoff\n" +
		`{"status":"ok","body":"output","memory_summary":"summary","satisfies":["ac-1","ac-3","ac-999"]}` +
		"\n```"
	ext.ingestHandoff(m, "child-1", cur, "extract", text, "")

	rows := indexByID(m.ACSnapshot())
	if rows["ac-1"].Status != ACSatisfied {
		t.Errorf("ac-1 status=%q, want satisfied", rows["ac-1"].Status)
	}
	if rows["ac-3"].Status != ACSatisfied {
		t.Errorf("ac-3 status=%q, want satisfied", rows["ac-3"].Status)
	}
	if rows["ac-2"].Status != ACUnsatisfied {
		t.Errorf("ac-2 status=%q (unmentioned should stay unsatisfied)", rows["ac-2"].Status)
	}
	wantEvidence := "worker data-analyst handoff iter-2 wave-extract"
	if rows["ac-1"].LastEvidence != wantEvidence {
		t.Errorf("ac-1 evidence=%q, want %q", rows["ac-1"].LastEvidence, wantEvidence)
	}
	if rows["ac-1"].SatisfiedAtIter != 2 {
		t.Errorf("ac-1 SatisfiedAtIter=%d, want 2", rows["ac-1"].SatisfiedAtIter)
	}
}

// TestIngestHandoff_SatisfiesDoesNotOverrideAlreadySatisfied
// verifies the worker-claim hierarchy: once a row is satisfied
// (by checker or earlier worker), a later worker re-claiming it
// does NOT overwrite the existing evidence + iter stamps.
func TestIngestHandoff_SatisfiesDoesNotOverrideAlreadySatisfied(t *testing.T) {
	ext := newPlannerExtension()
	state := newRenderedFakeState("mis-satisfies-already", productionRenderer(t))
	installMissionState(&state.fakeState)
	m := FromState(state)
	m.SeedAC([]ACAddSpec{{Statement: "ac-1"}}, OriginManifest)
	if err := m.ApplyStatusOnly([]ACUpdateSpec{
		{ID: "ac-1", Status: ACSatisfied, Evidence: "checker iter-1"},
	}, 1, "checker iter-1"); err != nil {
		t.Fatalf("ApplyStatusOnly: %v", err)
	}
	m.IterationCounter = 2
	m.BeginWave("rerun")
	cur := workerCursor{Name: "wkr", Role: "x", Skill: "y"}
	m.RegisterWorker("child-2", cur)
	text := "```handoff\n" +
		`{"status":"ok","body":"output","satisfies":["ac-1"]}` +
		"\n```"
	ext.ingestHandoff(m, "child-2", cur, "rerun", text, "")
	rows := indexByID(m.ACSnapshot())
	if !strings.Contains(rows["ac-1"].LastEvidence, "checker iter-1") {
		t.Errorf("worker overwrote checker evidence: %q", rows["ac-1"].LastEvidence)
	}
	if rows["ac-1"].SatisfiedAtIter != 1 {
		t.Errorf("worker overwrote SatisfiedAtIter: %d", rows["ac-1"].SatisfiedAtIter)
	}
}
