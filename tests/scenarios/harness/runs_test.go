//go:build duckdb_arrow && scenario

package harness

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadRuns_HappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runs.yaml")
	body := `runs:
  - name: claude-sonnet-embedded
    llm: configs/llm-claude-sonnet.yaml
    topology: configs/topology-embedded.yaml
    requires: []
    scenarios:
      - single_explorer
      - delegation_required
  - name: gemini-pro-hugr
    llm: configs/llm-gemini-pro.yaml
    topology: configs/topology-external-hugr.yaml
    requires: [hugr, gemini]
    scenarios:
      - full_analyst_workflow
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	rf, err := LoadRuns(path)
	if err != nil {
		t.Fatalf("LoadRuns: %v", err)
	}
	if len(rf.Runs) != 2 {
		t.Fatalf("len(Runs) = %d", len(rf.Runs))
	}
	if rf.Runs[0].LLM != "configs/llm-claude-sonnet.yaml" {
		t.Errorf("Runs[0].LLM = %q", rf.Runs[0].LLM)
	}
	if got, want := rf.Runs[1].Requires, []string{"hugr", "gemini"}; got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Runs[1].Requires = %v", got)
	}
}

func TestLoadRuns_RejectsMissingFields(t *testing.T) {
	tcs := []struct {
		name string
		body string
		want string
	}{
		{"no name", `runs:
  - llm: l.yaml
    topology: t.yaml
    scenarios: [a]
`, "name is required"},
		{"no llm", `runs:
  - name: r
    topology: t.yaml
    scenarios: [a]
`, "llm is required"},
		{"no scenarios", `runs:
  - name: r
    llm: l.yaml
    topology: t.yaml
`, "scenarios is empty"},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "runs.yaml")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadRuns(path); err == nil || !contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestLoadScenario_HappyPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scenario.yaml")
	body := `name: single_explorer
requires: [hugr]
steps:
  - say: "Hello"
    queries:
      - name: tool_calls_root
        graphql: |
          query ($sid: String!) { hub { db { agent { session_events(filter: {session_id: {eq: $sid}}) { seq } } } } }
        vars: { sid: "$sid" }
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := LoadScenario(path, "fallback-name")
	if err != nil {
		t.Fatalf("LoadScenario: %v", err)
	}
	if s.Name != "single_explorer" {
		t.Errorf("Name = %q", s.Name)
	}
	if len(s.Steps) != 1 || s.Steps[0].Say == "" {
		t.Errorf("Steps[0] = %+v", s.Steps[0])
	}
	if len(s.Steps[0].Queries) != 1 {
		t.Fatalf("Queries len = %d", len(s.Steps[0].Queries))
	}
	if s.Steps[0].Queries[0].Vars["sid"] != "$sid" {
		t.Errorf("expected literal $sid in vars, got %v", s.Steps[0].Queries[0].Vars)
	}
}

func TestLoadScenario_RejectsStepWithoutSayOrTick(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scenario.yaml")
	body := `name: bad
steps:
  - queries: []
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadScenario(path, "")
	if err == nil || !contains(err.Error(), "must have either say:") {
		t.Errorf("err = %v", err)
	}
}

func TestLoadScenario_ParsesInquiryResponses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scenario.yaml")
	body := `name: hitl
steps:
  - say: "go"
    inquiry_responses:
      - match:
          type: clarification
          question_contains: revenue
        respond:
          response: "by revenue"
        delay: 200ms
      - match:
          type: approval
        respond:
          approved: false
          reason: "denied by harness"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := LoadScenario(path, "hitl")
	if err != nil {
		t.Fatalf("LoadScenario: %v", err)
	}
	if len(s.Steps) != 1 {
		t.Fatalf("Steps len = %d", len(s.Steps))
	}
	rules := s.Steps[0].InquiryResponses
	if len(rules) != 2 {
		t.Fatalf("InquiryResponses len = %d", len(rules))
	}
	if rules[0].Match.Type != "clarification" {
		t.Errorf("rule[0] type = %q", rules[0].Match.Type)
	}
	if rules[0].Match.QuestionContains != "revenue" {
		t.Errorf("rule[0] question_contains = %q", rules[0].Match.QuestionContains)
	}
	if rules[0].Respond.Response != "by revenue" {
		t.Errorf("rule[0] response = %q", rules[0].Respond.Response)
	}
	if rules[0].Delay.Std() != 200*time.Millisecond {
		t.Errorf("rule[0] delay = %s", rules[0].Delay)
	}
	if rules[1].Match.Type != "approval" {
		t.Errorf("rule[1] type = %q", rules[1].Match.Type)
	}
	if rules[1].Respond.Approved == nil || *rules[1].Respond.Approved {
		t.Errorf("rule[1] approved = %v (want pointer to false)", rules[1].Respond.Approved)
	}
	if rules[1].Respond.Reason != "denied by harness" {
		t.Errorf("rule[1] reason = %q", rules[1].Respond.Reason)
	}
}

func TestLoadScenario_NameDefaultsToDirBasename(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scenario.yaml")
	body := `steps:
  - say: "x"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := LoadScenario(path, "fallback-name")
	if err != nil {
		t.Fatalf("LoadScenario: %v", err)
	}
	if s.Name != "fallback-name" {
		t.Errorf("Name = %q, want fallback-name", s.Name)
	}
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
