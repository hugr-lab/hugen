package template

import (
	"encoding/json"
	"testing"
)

func TestApply_Placeholders(t *testing.T) {
	ctx := Context{
		UserID:    "u1",
		Role:      "admin",
		AgentID:   "agent-7",
		SessionID: "sess-42",
		SessionMetadata: map[string]string{
			"workspace": "/var/agents/agent-7/workspace",
			"team":      "growth",
		},
	}

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"user_id", `{"u": "[$auth.user_id]"}`, `{"u":"u1"}`},
		{"role", `{"r": "[$auth.role]"}`, `{"r":"admin"}`},
		{"agent_id", `{"a": "[$agent.id]"}`, `{"a":"agent-7"}`},
		{"session_id", `{"s": "[$session.id]"}`, `{"s":"sess-42"}`},
		{
			"session_metadata",
			`{"w": "[$session.metadata.workspace]"}`,
			`{"w":"/var/agents/agent-7/workspace"}`,
		},
		{
			"interpolation_inside_string",
			`{"path": "[$session.metadata.workspace]/data/x.parquet"}`,
			`{"path":"/var/agents/agent-7/workspace/data/x.parquet"}`,
		},
		{
			"unknown_token_preserved",
			`{"x": "[$unknown.thing]"}`,
			`{"x":"[$unknown.thing]"}`,
		},
		{
			"missing_metadata_key_empty_string",
			`{"x": "[$session.metadata.absent]"}`,
			`{"x":""}`,
		},
		{
			"unclosed_bracket_preserved",
			`{"x": "[$auth.user_id"}`,
			`{"x":"[$auth.user_id"}`,
		},
		{
			"no_placeholder_passthrough",
			`{"x": "plain"}`,
			`{"x":"plain"}`,
		},
		{
			"no_recursion",
			// Substituting user_id="[$agent.id]" must NOT then resolve
			// agent.id; the produced text is preserved verbatim.
			`{"x": "[$auth.user_id]"}`,
			`{"x":"[$agent.id]"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := ctx
			if tc.name == "no_recursion" {
				c.UserID = "[$agent.id]"
			}
			out, err := Apply(json.RawMessage(tc.in), c)
			if err != nil {
				t.Fatalf("Apply error: %v", err)
			}
			if string(out) != tc.want {
				t.Fatalf("Apply(%s)\n  got:  %s\n  want: %s", tc.in, out, tc.want)
			}
		})
	}
}

func TestApply_NestedStructures(t *testing.T) {
	ctx := Context{UserID: "u1", AgentID: "a1"}
	in := `{"args":{"path":"/x/[$agent.id]/y","tags":["[$auth.user_id]","raw"]},"n":42,"b":true}`
	want := `{"args":{"path":"/x/a1/y","tags":["u1","raw"]},"b":true,"n":42}`

	out, err := Apply(json.RawMessage(in), ctx)
	if err != nil {
		t.Fatalf("Apply error: %v", err)
	}
	// JSON object key order is non-deterministic; round-trip
	// through unmarshal+remarshal so we're comparing structures.
	var got, exp any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal got: %v", err)
	}
	if err := json.Unmarshal([]byte(want), &exp); err != nil {
		t.Fatalf("unmarshal want: %v", err)
	}
	if !jsonEqual(got, exp) {
		t.Fatalf("Apply mismatch\n  got:  %s\n  want: %s", out, want)
	}
}

func TestApply_EmptyInput(t *testing.T) {
	out, err := Apply(nil, Context{})
	if err != nil {
		t.Fatalf("Apply(nil) error: %v", err)
	}
	if out != nil {
		t.Fatalf("Apply(nil) = %s, want nil", out)
	}
}

func TestApply_InvalidJSON(t *testing.T) {
	_, err := Apply(json.RawMessage(`{not json}`), Context{})
	if err == nil {
		t.Fatal("Apply(invalid JSON) returned nil error")
	}
}

func TestApplyString(t *testing.T) {
	ctx := Context{UserID: "u1", AgentID: "a1"}
	got := ApplyString("filter: user_id = '[$auth.user_id]' AND agent = '[$agent.id]'", ctx)
	want := "filter: user_id = 'u1' AND agent = 'a1'"
	if got != want {
		t.Fatalf("ApplyString:\n  got:  %s\n  want: %s", got, want)
	}
}

func jsonEqual(a, b any) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}
