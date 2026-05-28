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

// TestParseHandoff_KindResearch_MultiPropagates verifies the
// `multi: true` flag on a clarification flows through
// ParseHandoff → DecodeResearchOutput → ResearchClarification.Multi
// without being silently dropped. Regression guard: prior to the
// Multi field being declared on ResearchClarification, the JSON
// decoder ignored the value and the TUI rendered the question as
// a single-select list with no checkbox markers.
func TestParseHandoff_KindResearch_MultiPropagates(t *testing.T) {
	raw := "```research\n" + `{"status":"ok","body":{
		"done": false,
		"clarifications": [
			{"id":"focus","question":"Which datasets?","kind":"required","options":["A","B","C"],"multi":true},
			{"id":"format","question":"Output format?","kind":"required","options":["html","pdf"]}
		],
		"memory_summary": "asking scope + format"
	}}` + "\n```"
	h, err := ParseHandoff(raw)
	if err != nil {
		t.Fatalf("ParseHandoff: %v", err)
	}
	out, decodeErr := DecodeResearchOutput(h)
	if decodeErr != nil {
		t.Fatalf("DecodeResearchOutput: %v", decodeErr)
	}
	if !out.Clarifications[0].Multi {
		t.Errorf("clarifications[0].Multi = false, want true (the `multi: true` was lost in decode)")
	}
	if out.Clarifications[1].Multi {
		t.Errorf("clarifications[1].Multi = true, want false (no multi flag on the second entry)")
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

