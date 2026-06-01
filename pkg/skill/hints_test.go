package skill

import (
	"errors"
	"strings"
	"testing"
)

// hintManifest builds a minimal manifest source carrying one
// metadata.hugen.hints entry, for Parse-level tests.
func hintManifest(hintsYAML string) string {
	return `---
name: data-helper
description: A skill with in-turn hints.
license: MIT
metadata:
  hugen:
    tier_compatibility: [worker]
    hints:
` + hintsYAML + `
---
body
`
}

// TestParse_Hint_OnToolError_AliasNormalizes pins the deprecated alias:
// a manifest declaring the legacy `on_tool_error` type parses and is
// normalised to the single canonical `on_tool_result` type (so the
// runtime's one advisor sees it), with the regex compiled at parse.
func TestParse_Hint_OnToolError_AliasNormalizes(t *testing.T) {
	src := hintManifest(`      - type: on_tool_error
        tools: ["hugr-main:data-*"]
        match: "Cannot query field .*_aggregation"
        message: "Use discovery first."`)
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(m.Hugen.Hints) != 1 {
		t.Fatalf("want 1 hint, got %d", len(m.Hugen.Hints))
	}
	h := m.Hugen.Hints[0]
	if h.Type != HintTypeOnToolResult {
		t.Errorf("type = %q, want it normalised to %q", h.Type, HintTypeOnToolResult)
	}
	if h.re == nil {
		t.Errorf("match regex should be compiled at parse")
	}
}

func TestParse_Hint_InvalidRegex_FailsLoud(t *testing.T) {
	src := hintManifest(`      - type: on_tool_result
        match: "Cannot query field ([unclosed"
        message: "x"`)
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatalf("Parse should reject an invalid hint regex")
	}
	if !errors.Is(err, ErrManifestInvalid) {
		t.Errorf("want ErrManifestInvalid, got %v", err)
	}
	if !strings.Contains(err.Error(), "invalid regexp") {
		t.Errorf("error should name the bad regex: %v", err)
	}
}

func TestParse_Hint_MissingType_FailsLoud(t *testing.T) {
	src := hintManifest(`      - message: "no type"`)
	if _, err := Parse([]byte(src)); err == nil {
		t.Fatalf("Parse should reject a hint with no type")
	}
}

func TestParse_Hint_MissingMessage_FailsLoud(t *testing.T) {
	src := hintManifest(`      - type: on_tool_result
        match: "x"`)
	if _, err := Parse([]byte(src)); err == nil {
		t.Fatalf("Parse should reject an on_tool_result hint with no message")
	}
}

func TestParse_Hint_UnknownType_ToleratedForwardCompat(t *testing.T) {
	src := hintManifest(`      - type: pre_tool_call
        message: "future variation"`)
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("unknown hint type must parse (forward-compat), got %v", err)
	}
	if len(m.Hugen.Hints) != 1 {
		t.Fatalf("unknown-type hint should be kept, got %d", len(m.Hugen.Hints))
	}
	// And it never matches a result consult.
	if msg := m.Hugen.Hints[0].MatchToolResult("any:tool", "", "boom"); msg != "" {
		t.Errorf("unknown-type hint matched a result consult: %q", msg)
	}
}

// TestHint_MatchToolResult exercises the single match path on every
// shape the runtime feeds it: a runtime error message (code set), a
// success-envelope failure body (code empty), a truncated-but-clean
// body, and structured-code matching. The runtime never pre-classifies;
// the hint's tool glob + Code + regex over ResultText do all the work.
func TestHint_MatchToolResult(t *testing.T) {
	const guidance = "Enumerate modules via discovery-search_modules first."
	errHint := Hint{
		Type:    HintTypeOnToolResult,
		Tools:   []string{"hugr-main:data-*"},
		Match:   "Cannot query field .*_aggregation",
		Message: guidance,
	}
	const truncGuidance = "switch to hugr-query file output"
	truncHint := Hint{
		Type:    HintTypeOnToolResult,
		Tools:   []string{"hugr-main:data-inline_graphql_result"},
		Match:   `is_truncated"?\s*:\s*true`,
		Message: truncGuidance,
	}

	cases := []struct {
		name             string
		hint             Hint
		tool, code, text string
		want             string
	}{
		{
			name: "match on runtime error message (code set)",
			hint: errHint,
			tool: "hugr-main:data-inline_graphql_result",
			code: "validation",
			text: `Cannot query field "core_modules_aggregation" on type "Query"`,
			want: guidance,
		},
		{
			name: "match on success-envelope failure body (no code)",
			hint: errHint,
			tool: "hugr-main:data-inline_graphql_result",
			text: `{"is_error":true,"text":"Cannot query field \"x_aggregation\""}`,
			want: guidance,
		},
		{
			name: "match against model-visible underscore tool form",
			hint: errHint,
			tool: "hugr-main_data-inline_graphql_result",
			text: `Cannot query field "y_aggregation"`,
			want: guidance,
		},
		{
			name: "tool glob excludes other providers",
			hint: errHint,
			tool: "bash:run",
			text: `Cannot query field "z_aggregation"`,
			want: "",
		},
		{
			name: "text regex excludes non-matching error",
			hint: errHint,
			tool: "hugr-main:data-inline_graphql_result",
			text: `unknown filter operator`,
			want: "",
		},
		{
			name: "empty tools matches any tool",
			hint: Hint{Type: HintTypeOnToolResult, Match: "boom", Message: guidance},
			tool: "anything:at-all",
			text: "boom happened",
			want: guidance,
		},
		{
			name: "empty match matches any result for the tool",
			hint: Hint{Type: HintTypeOnToolResult, Tools: []string{"bash:*"}, Message: guidance},
			tool: "bash:run",
			text: "exit status 1",
			want: guidance,
		},
		{
			name: "code match",
			hint: Hint{Type: HintTypeOnToolResult, Code: "not_found", Message: guidance},
			tool: "x:y",
			code: "not_found",
			text: "tool not in snapshot",
			want: guidance,
		},
		{
			name: "code mismatch",
			hint: Hint{Type: HintTypeOnToolResult, Code: "not_found", Message: guidance},
			tool: "x:y",
			code: "io",
			text: "boom",
			want: "",
		},
		{
			name: "code-bearing hint does not fire on a successful dispatch (empty code)",
			hint: Hint{Type: HintTypeOnToolResult, Code: "not_found", Message: guidance},
			tool: "x:y",
			text: "not_found appears in the data",
			want: "",
		},
		{
			name: "truncated success body matches the truncation hint",
			hint: truncHint,
			tool: "hugr-main:data-inline_graphql_result",
			text: `{"data":{"x":1},"is_truncated":true}`,
			want: truncGuidance,
		},
		{
			name: "non-truncated success does not match",
			hint: truncHint,
			tool: "hugr-main:data-inline_graphql_result",
			text: `{"data":{"x":1},"is_truncated":false}`,
			want: "",
		},
		{
			name: "un-normalised legacy on_tool_error literal never matches",
			hint: Hint{Type: HintTypeOnToolError, Match: "is_truncated", Message: guidance},
			tool: "hugr-main:data-inline_graphql_result",
			text: `{"is_truncated":true}`,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.hint.MatchToolResult(tc.tool, tc.code, tc.text)
			if got != tc.want {
				t.Errorf("MatchToolResult = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParse_Hint_OnToolResult_RoundTrips(t *testing.T) {
	src := hintManifest(`      - type: on_tool_result
        tools: ["hugr-main:data-inline_graphql_result"]
        match: 'is_truncated"?\s*:\s*true'
        message: "switch to file output"`)
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(m.Hugen.Hints) != 1 {
		t.Fatalf("hints = %d, want 1", len(m.Hugen.Hints))
	}
	h := m.Hugen.Hints[0]
	if h.Type != HintTypeOnToolResult {
		t.Errorf("type = %q, want %q", h.Type, HintTypeOnToolResult)
	}
	// Compiled at parse — must match a truncated success body.
	if msg := h.MatchToolResult("hugr-main:data-inline_graphql_result", "",
		`{"data":{...},"is_truncated":true}`); msg != "switch to file output" {
		t.Errorf("MatchToolResult = %q, want guidance", msg)
	}
}
