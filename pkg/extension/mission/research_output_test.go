package mission

import (
	"strings"
	"testing"
)

func TestParseHandoff_KindResearch_Terminal(t *testing.T) {
	raw := "```research\n" +
		`{"status":"ok","body":{"findings":"Source op2023 carries payments + ownership; join key is contract_id.","resolved_user_inputs":{"file_path":"~/Downloads/op2023.html"},"memory_summary":"scoped to op2023 sources"}}` +
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
	if !strings.Contains(out.Findings, "op2023") {
		t.Errorf("Findings = %q, want substring 'op2023'", out.Findings)
	}
	if out.ResolvedUserInputs["file_path"] != "~/Downloads/op2023.html" {
		t.Errorf("ResolvedUserInputs[file_path] = %v", out.ResolvedUserInputs["file_path"])
	}
	if out.MemorySummary != "scoped to op2023 sources" {
		t.Errorf("MemorySummary = %q", out.MemorySummary)
	}
}

func TestParseHandoff_KindResearch_ACProposals(t *testing.T) {
	raw := "```research\n" + `{"status":"ok","body":{
		"findings":"Confirmed tf.roads has length_m + geozone_id.",
		"ac_proposals":[
			{"statement":"Roads reported per geozone","rationale":"user picked breakdown by geozone"},
			{"statement":"Total length in km"}
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
	if len(out.ACProposals) != 2 {
		t.Fatalf("ACProposals = %d, want 2", len(out.ACProposals))
	}
	if out.ACProposals[0].Statement != "Roads reported per geozone" {
		t.Errorf("ACProposals[0].Statement = %q", out.ACProposals[0].Statement)
	}
	if out.ACProposals[0].Rationale != "user picked breakdown by geozone" {
		t.Errorf("ACProposals[0].Rationale = %q", out.ACProposals[0].Rationale)
	}
}

func TestValidateRequired_Research_RejectsMissingFindings(t *testing.T) {
	raw := "```research\n" + `{"status":"ok","body":{"memory_summary":"did stuff"}}` + "\n```"
	_, err := ParseHandoff(raw)
	if err == nil {
		t.Fatal("ParseHandoff: want error, got nil")
	}
	if !strings.Contains(err.Error(), "findings") {
		t.Errorf("err = %v, want substring 'findings'", err)
	}
}

func TestValidateRequired_Research_RejectsEmptyFindings(t *testing.T) {
	raw := "```research\n" + `{"status":"ok","body":{"findings":"   "}}` + "\n```"
	_, err := ParseHandoff(raw)
	if err == nil {
		t.Fatal("ParseHandoff: want error, got nil")
	}
	if !strings.Contains(err.Error(), "findings") {
		t.Errorf("err = %v, want substring 'findings'", err)
	}
}
