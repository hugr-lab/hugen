package mission

import (
	"strings"
	"testing"
)

func TestParseHandoff_KindResearch_DoneTrue(t *testing.T) {
	raw := "```research\n" +
		`{"status":"ok","body":{"done":true,"findings":"Source op2023 carries payments + ownership.","resolved_user_inputs":{"file_path":"~/Downloads/op2023.html"},"memory_summary":"scoped to op2023 sources"}}` +
		"\n```"
	h, err := ParseHandoff(raw)
	if err != nil {
		t.Fatalf("ParseHandoff: %v", err)
	}
	if h.Kind != KindResearch {
		t.Fatalf("Kind = %q, want %q", h.Kind, KindResearch)
	}
	out, decodeErr := DecodeResearchOutput(h)
	if decodeErr != nil {
		t.Fatalf("DecodeResearchOutput: %v", decodeErr)
	}
	if !out.Done {
		t.Fatal("Done = false, want true")
	}
	if !strings.Contains(out.Findings, "op2023") {
		t.Errorf("Findings = %q, want substring 'op2023'", out.Findings)
	}
	if out.ResolvedUserInputs["file_path"] != "~/Downloads/op2023.html" {
		t.Errorf("ResolvedUserInputs[file_path] = %v", out.ResolvedUserInputs["file_path"])
	}
}

func TestParseHandoff_KindResearch_DoneFalseWithClarifications(t *testing.T) {
	raw := "```research\n" + `{"status":"ok","body":{
		"done": false,
		"clarifications": [
			{"id":"file_path","question":"Where to save?","kind":"required","options":["~/Downloads/x.html"]},
			{"id":"scope","question":"Date range?","kind":"optional"},
			{"id":"notes","question":"Anything else?","kind":"comment"}
		],
		"memory_summary": "asking for path + scope"
	}}` + "\n```"
	h, err := ParseHandoff(raw)
	if err != nil {
		t.Fatalf("ParseHandoff: %v", err)
	}
	out, decodeErr := DecodeResearchOutput(h)
	if decodeErr != nil {
		t.Fatalf("DecodeResearchOutput: %v", decodeErr)
	}
	if out.Done {
		t.Fatal("Done = true, want false")
	}
	if len(out.Clarifications) != 3 {
		t.Fatalf("Clarifications = %d, want 3", len(out.Clarifications))
	}
	if out.Clarifications[0].ID != "file_path" {
		t.Errorf("clarifications[0].ID = %q, want file_path", out.Clarifications[0].ID)
	}
	if out.Clarifications[2].Kind != "comment" {
		t.Errorf("clarifications[2].Kind = %q, want comment", out.Clarifications[2].Kind)
	}
}

func TestParseHandoff_KindResearch_AutoAssignIDs(t *testing.T) {
	// Role omitted ids — DecodeResearchOutput auto-assigns q1/q2.
	raw := "```research\n" + `{"status":"ok","body":{
		"done": false,
		"clarifications": [
			{"question":"Q1?","kind":"required"},
			{"question":"Q2?","kind":"required"}
		]
	}}` + "\n```"
	h, err := ParseHandoff(raw)
	if err != nil {
		t.Fatalf("ParseHandoff: %v", err)
	}
	out, decodeErr := DecodeResearchOutput(h)
	if decodeErr != nil {
		t.Fatalf("DecodeResearchOutput: %v", decodeErr)
	}
	if out.Clarifications[0].ID != "q1" || out.Clarifications[1].ID != "q2" {
		t.Errorf("auto-assigned ids: got %q,%q want q1,q2", out.Clarifications[0].ID, out.Clarifications[1].ID)
	}
}

func TestValidateRequired_Research_RejectsDoneFalseWithoutClarifications(t *testing.T) {
	raw := "```research\n" + `{"status":"ok","body":{"done":false}}` + "\n```"
	_, err := ParseHandoff(raw)
	if err == nil {
		t.Fatal("ParseHandoff: want error, got nil")
	}
	if !strings.Contains(err.Error(), "clarifications") {
		t.Errorf("err = %v, want substring 'clarifications'", err)
	}
}

func TestValidateRequired_Research_RejectsDoneTrueWithoutFindings(t *testing.T) {
	raw := "```research\n" + `{"status":"ok","body":{"done":true}}` + "\n```"
	_, err := ParseHandoff(raw)
	if err == nil {
		t.Fatal("ParseHandoff: want error, got nil")
	}
	if !strings.Contains(err.Error(), "findings") {
		t.Errorf("err = %v, want substring 'findings'", err)
	}
}

func TestValidateRequired_Research_RejectsDuplicateIDs(t *testing.T) {
	raw := "```research\n" + `{"status":"ok","body":{
		"done": false,
		"clarifications": [
			{"id":"same","question":"Q1"},
			{"id":"same","question":"Q2"}
		]
	}}` + "\n```"
	_, err := ParseHandoff(raw)
	if err == nil {
		t.Fatal("ParseHandoff: want error, got nil (duplicate ids)")
	}
	if !strings.Contains(err.Error(), "duplicates") {
		t.Errorf("err = %v, want substring 'duplicates'", err)
	}
}

func TestValidateRequired_Research_RejectsUnknownKind(t *testing.T) {
	raw := "```research\n" + `{"status":"ok","body":{
		"done": false,
		"clarifications": [{"id":"x","question":"Q","kind":"super_required"}]
	}}` + "\n```"
	_, err := ParseHandoff(raw)
	if err == nil {
		t.Fatal("ParseHandoff: want error, got nil")
	}
	if !strings.Contains(err.Error(), "super_required") {
		t.Errorf("err = %v, want substring 'super_required'", err)
	}
}

func TestAutoResearchHeuristic(t *testing.T) {
	cases := []struct {
		name string
		goal string
		want bool
	}{
		{"empty triggers", "", true},
		{"short goal triggers", "Анализ", true},
		{"long concrete goal — no trigger", "List the providers table by total payments grouped by year for the op2023 source filtered to 2024", false},
		{"deliverable keyword english", "Build a report and save the html somewhere reasonable", true},
		{"deliverable keyword russian", "сделай отчёт по продажам за квартал", true},
		{"pronoun standalone triggers", "Analyse it for me please during this windowed period", true},
		{"trigger phrase english", "help me figure out which table holds payments for vendor X", true},
		{"trigger phrase russian", "помоги разобраться какие данные есть в op2023", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := autoResearchHeuristic(tc.goal, MissionManifest{})
			if got != tc.want {
				t.Errorf("autoResearchHeuristic(%q) = %v, want %v", tc.goal, got, tc.want)
			}
		})
	}
}

func TestShouldRunResearch_Predicate(t *testing.T) {
	cases := []struct {
		name     string
		research *ResearchManifest
		goal     string
		want     bool
	}{
		{"nil manifest", nil, "any goal", false},
		{"always", &ResearchManifest{When: ResearchWhenAlways}, "trivial concrete goal that wouldn't trigger auto", true},
		{"auto trips on deliverable", &ResearchManifest{When: ResearchWhenAuto}, "save this as html", true},
		{"auto skips concrete long goal", &ResearchManifest{When: ResearchWhenAuto}, "List the providers table for op2023 source grouped by year of payment date", false},
		{"if_goal_matches hits", &ResearchManifest{When: ResearchWhenIfGoalMatches, Predicate: `(?i)schedule`}, "schedule daily report", true},
		{"if_goal_matches misses", &ResearchManifest{When: ResearchWhenIfGoalMatches, Predicate: `(?i)schedule`}, "list providers", false},
		{"if_goal_matches broken regex fails closed", &ResearchManifest{When: ResearchWhenIfGoalMatches, Predicate: `(?P<broken`}, "anything", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manifest := MissionManifest{Research: tc.research}
			got := shouldRunResearch(manifest, tc.goal)
			if got != tc.want {
				t.Errorf("shouldRunResearch(%+v, %q) = %v, want %v", tc.research, tc.goal, got, tc.want)
			}
		})
	}
}
