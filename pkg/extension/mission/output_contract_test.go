package mission

import (
	"strings"
	"testing"
)

func TestParseHandoff_Kind(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantOk  bool
		wantErr string
		check   func(t *testing.T, h Handoff)
	}{
		{
			name: "handoff JSON inside ```handoff fence",
			raw: "Some prose first.\n\n```handoff\n" +
				`{"status":"ok","body":"hello world","memory_summary":"echoed"}` +
				"\n```\nMore prose after.",
			wantOk: true,
			check: func(t *testing.T, h Handoff) {
				if h.Kind != KindHandoff {
					t.Errorf("Kind = %q, want handoff", h.Kind)
				}
				if h.Status != "ok" {
					t.Errorf("Status = %q, want ok", h.Status)
				}
				if h.MemorySummary != "echoed" {
					t.Errorf("MemorySummary = %q, want echoed", h.MemorySummary)
				}
				if s, _ := h.Body.(string); s != "hello world" {
					t.Errorf("Body = %v, want hello world", h.Body)
				}
			},
		},
		{
			name: "plan kind requires next_wave key",
			raw: "```plan\n" +
				`{"status":"ok","body":{"rationale":"missing next_wave","roadmap":[]}}` +
				"\n```",
			wantErr: "next_wave",
		},
		{
			name: "plan kind requires roadmap key",
			raw: "```plan\n" +
				`{"status":"ok","body":{"next_wave":null,"rationale":"r"}}` +
				"\n```",
			wantErr: "roadmap",
		},
		{
			name: "plan kind requires rationale key",
			raw: "```plan\n" +
				`{"status":"ok","body":{"next_wave":null,"roadmap":[]}}` +
				"\n```",
			wantErr: "rationale",
		},
		{
			name: "plan kind accepts next_wave: null (plan_complete signal)",
			raw: "```plan\n" +
				`{"status":"ok","body":{"next_wave":null,"roadmap":[],"rationale":"all done"}}` +
				"\n```",
			wantOk: true,
			check: func(t *testing.T, h Handoff) {
				if h.Kind != KindPlan {
					t.Errorf("Kind = %q, want plan", h.Kind)
				}
				p, err := DecodePlan(h)
				if err != nil {
					t.Fatalf("DecodePlan: %v", err)
				}
				if p != nil {
					t.Errorf("DecodePlan: got %+v, want nil (plan_complete signal)", p)
				}
			},
		},
		{
			name: "plan kind: next_wave missing label",
			raw: "```plan\n" +
				`{"status":"ok","body":{"next_wave":{"subagents":[{"name":"w1"}]},"roadmap":[],"rationale":"r"}}` +
				"\n```",
			wantErr: "next_wave.label",
		},
		{
			name: "plan kind: next_wave missing subagents",
			raw: "```plan\n" +
				`{"status":"ok","body":{"next_wave":{"label":"wave-1"},"roadmap":[],"rationale":"r"}}` +
				"\n```",
			wantErr: "next_wave.subagents",
		},
		{
			name: "plan kind: full happy path round-trips through DecodePlan",
			raw: "```plan\n" +
				`{"status":"ok","body":{"mission_goal":"map tables","mission_acceptance_criteria":["discovery produces table list"],"next_wave":{"label":"discover","subagents":[{"name":"explorer","role":"schema-explorer","task":"map tables"}]},"roadmap":["analyse"],"rationale":"discover first"}}` +
				"\n```",
			wantOk: true,
			check: func(t *testing.T, h Handoff) {
				p, err := DecodePlan(h)
				if err != nil {
					t.Fatalf("DecodePlan: %v", err)
				}
				if p == nil {
					t.Fatal("DecodePlan returned nil; want a Plan")
				}
				if p.NextWave.Label != "discover" {
					t.Errorf("NextWave.Label = %q, want discover", p.NextWave.Label)
				}
				if len(p.NextWave.Subagents) != 1 || p.NextWave.Subagents[0].Name != "explorer" {
					t.Errorf("NextWave.Subagents = %+v", p.NextWave.Subagents)
				}
				if p.Rationale != "discover first" {
					t.Errorf("Rationale = %q", p.Rationale)
				}
				if len(p.Roadmap) != 1 || p.Roadmap[0].Label != "analyse" {
					t.Errorf("Roadmap = %v", p.Roadmap)
				}
			},
		},
		{
			name: "verdict kind requires decision",
			raw: "```verdict\n" +
				`{"status":"ok","body":{"issues":["x"]}}` +
				"\n```",
			wantErr: "decision",
		},
		{
			name: "verdict kind rejects unknown decision",
			raw: "```verdict\n" +
				`{"status":"ok","body":{"decision":"yolo"}}` +
				"\n```",
			wantErr: "unknown decision",
		},
		{
			name: "verdict happy path: continue + DecodeVerdict",
			raw: "```verdict\n" +
				`{"status":"ok","body":{"decision":"continue"}}` +
				"\n```",
			wantOk: true,
			check: func(t *testing.T, h Handoff) {
				if h.Kind != KindVerdict {
					t.Errorf("Kind = %q, want verdict", h.Kind)
				}
				v, err := DecodeVerdict(h)
				if err != nil {
					t.Fatalf("DecodeVerdict: %v", err)
				}
				if v.Decision != VerdictContinue {
					t.Errorf("Decision = %q, want continue", v.Decision)
				}
			},
		},
		{
			name: "verdict amend carries issues",
			raw: "```verdict\n" +
				`{"status":"ok","body":{"decision":"amend","issues":["missing table","wrong filter"]}}` +
				"\n```",
			wantOk: true,
			check: func(t *testing.T, h Handoff) {
				v, err := DecodeVerdict(h)
				if err != nil {
					t.Fatalf("DecodeVerdict: %v", err)
				}
				if v.Decision != VerdictAmend {
					t.Errorf("Decision = %q, want amend", v.Decision)
				}
				if len(v.Issues) != 2 || v.Issues[0] != "missing table" {
					t.Errorf("Issues = %v", v.Issues)
				}
			},
		},
		{
			name: "verdict finish",
			raw: "```verdict\n" +
				`{"status":"ok","body":{"decision":"finish","reason":"goal met"}}` +
				"\n```",
			wantOk: true,
			check: func(t *testing.T, h Handoff) {
				v, _ := DecodeVerdict(h)
				if v.Decision != VerdictFinish {
					t.Errorf("Decision = %q, want finish", v.Decision)
				}
			},
		},
		{
			name: "verdict ac_update happy path",
			raw: "```verdict\n" +
				`{"status":"ok","body":{"decision":"continue","ac_update":[{"id":"ac-1","status":"satisfied","evidence":"wrk@x"}]}}` +
				"\n```",
			wantOk: true,
			check: func(t *testing.T, h Handoff) {
				v, err := DecodeVerdict(h)
				if err != nil {
					t.Fatalf("DecodeVerdict: %v", err)
				}
				if len(v.ACUpdate) != 1 || v.ACUpdate[0].ID != "ac-1" {
					t.Errorf("ACUpdate = %+v", v.ACUpdate)
				}
				if v.ACUpdate[0].Status != ACSatisfied {
					t.Errorf("status=%q", v.ACUpdate[0].Status)
				}
			},
		},
		{
			name: "verdict ac_update rejects statement (planner-only field)",
			raw: "```verdict\n" +
				`{"status":"ok","body":{"decision":"amend","ac_update":[{"id":"ac-1","statement":"rewrite","status":"unsatisfied"}]}}` +
				"\n```",
			wantErr: "checker cannot rewrite criteria",
		},
		{
			name: "verdict ac_update rejects drop (planner-only field)",
			raw: "```verdict\n" +
				`{"status":"ok","body":{"decision":"amend","ac_update":[{"id":"ac-1","drop":true}]}}` +
				"\n```",
			wantErr: "checker cannot drop criteria",
		},
		{
			name: "verdict ac_update rejects status=dropped wire",
			raw: "```verdict\n" +
				`{"status":"ok","body":{"decision":"amend","ac_update":[{"id":"ac-1","status":"dropped"}]}}` +
				"\n```",
			wantErr: "checker cannot drop criteria",
		},
		{
			name: "verdict ac_update requires status",
			raw: "```verdict\n" +
				`{"status":"ok","body":{"decision":"amend","ac_update":[{"id":"ac-1","evidence":"x"}]}}` +
				"\n```",
			wantErr: "status is required",
		},
		{
			name: "verdict ac_update requires id",
			raw: "```verdict\n" +
				`{"status":"ok","body":{"decision":"amend","ac_update":[{"status":"satisfied"}]}}` +
				"\n```",
			wantErr: "id is required",
		},
		{
			name: "synthesis happy path",
			raw: "```synthesis\n" +
				`{"status":"ok","body":"final answer here"}` +
				"\n```",
			wantOk: true,
			check: func(t *testing.T, h Handoff) {
				if h.Kind != KindSynthesis {
					t.Errorf("Kind = %q, want synthesis", h.Kind)
				}
			},
		},
		{
			name:    "no fence",
			raw:     "just prose with no fenced block",
			wantErr: "no fenced handoff block",
		},
		{
			name:    "empty",
			raw:     "",
			wantErr: "empty body",
		},
		{
			name: "status required",
			raw: "```handoff\n" +
				`{"body":"missing status"}` +
				"\n```",
			wantErr: "status is required",
		},
		{
			name: "non-ok status needs reason",
			raw: "```handoff\n" +
				`{"status":"error","body":"oops"}` +
				"\n```",
			wantErr: "reason is required",
		},
		{
			name: "json fence with inferred kind",
			raw: "```json\n" +
				`{"kind":"handoff","status":"ok","body":"x"}` +
				"\n```",
			wantOk: true,
			check: func(t *testing.T, h Handoff) {
				if h.Kind != KindHandoff {
					t.Errorf("inferred Kind = %q, want handoff", h.Kind)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, err := ParseHandoff(tc.raw)
			if tc.wantOk {
				if err != nil {
					t.Fatalf("ParseHandoff: unexpected error %v", err)
				}
				if tc.check != nil {
					tc.check(t, h)
				}
				return
			}
			if err == nil {
				t.Fatalf("ParseHandoff: want error containing %q, got nil", tc.wantErr)
			}
			if tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ParseHandoff: error = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestOutputContractKindKnown(t *testing.T) {
	good := []OutputContractKind{KindHandoff, KindPlan, KindVerdict, KindSynthesis}
	for _, k := range good {
		if !k.Known() {
			t.Errorf("Known(%q) = false, want true", k)
		}
	}
	if OutputContractKind("nope").Known() {
		t.Errorf("Known(nope) = true, want false")
	}
}
