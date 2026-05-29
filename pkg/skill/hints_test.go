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

func TestParse_Hint_OnToolError_RoundTrips(t *testing.T) {
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
	if h.Type != HintTypeOnToolError {
		t.Errorf("type = %q, want %q", h.Type, HintTypeOnToolError)
	}
	if h.re == nil {
		t.Errorf("match regex should be compiled at parse")
	}
}

func TestParse_Hint_InvalidRegex_FailsLoud(t *testing.T) {
	src := hintManifest(`      - type: on_tool_error
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

func TestParse_Hint_OnToolError_MissingMessage_FailsLoud(t *testing.T) {
	src := hintManifest(`      - type: on_tool_error
        match: "x"`)
	if _, err := Parse([]byte(src)); err == nil {
		t.Fatalf("Parse should reject an on_tool_error hint with no message")
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
	// And it never matches an on_tool_error consult.
	if msg := m.Hugen.Hints[0].MatchToolError("any:tool", "", "boom", ""); msg != "" {
		t.Errorf("unknown-type hint matched on_tool_error: %q", msg)
	}
}

func TestHint_MatchToolError(t *testing.T) {
	const guidance = "Enumerate modules via discovery-search_modules first."
	base := Hint{
		Type:    HintTypeOnToolError,
		Tools:   []string{"hugr-main:data-*"},
		Match:   "Cannot query field .*_aggregation",
		Message: guidance,
	}
	cases := []struct {
		name             string
		hint             Hint
		tool, code, msg  string
		resultText       string
		want             string
	}{
		{
			name: "match on runtime message",
			hint: base,
			tool: "hugr-main:data-inline_graphql_result",
			msg:  `Cannot query field "core_modules_aggregation" on type "Query"`,
			want: guidance,
		},
		{
			name:       "match on provider result body",
			hint:       base,
			tool:       "hugr-main:data-inline_graphql_result",
			resultText: `{"is_error":true,"text":"Cannot query field \"x_aggregation\""}`,
			want:       guidance,
		},
		{
			name: "match against model-visible underscore tool form",
			hint: base,
			tool: "hugr-main_data-inline_graphql_result",
			msg:  `Cannot query field "y_aggregation"`,
			want: guidance,
		},
		{
			name: "tool glob excludes other providers",
			hint: base,
			tool: "bash:run",
			msg:  `Cannot query field "z_aggregation"`,
			want: "",
		},
		{
			name: "text regex excludes non-matching error",
			hint: base,
			tool: "hugr-main:data-inline_graphql_result",
			msg:  `unknown filter operator`,
			want: "",
		},
		{
			name: "empty tools matches any tool",
			hint: Hint{Type: HintTypeOnToolError, Match: "boom", Message: guidance},
			tool: "anything:at-all",
			msg:  "boom happened",
			want: guidance,
		},
		{
			name: "empty match matches any error for the tool",
			hint: Hint{Type: HintTypeOnToolError, Tools: []string{"bash:*"}, Message: guidance},
			tool: "bash:run",
			msg:  "exit status 1",
			want: guidance,
		},
		{
			name: "code match",
			hint: Hint{Type: HintTypeOnToolError, Code: "not_found", Message: guidance},
			tool: "x:y",
			code: "not_found",
			msg:  "tool not in snapshot",
			want: guidance,
		},
		{
			name: "code mismatch",
			hint: Hint{Type: HintTypeOnToolError, Code: "not_found", Message: guidance},
			tool: "x:y",
			code: "io",
			msg:  "boom",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.hint.MatchToolError(tc.tool, tc.code, tc.msg, tc.resultText)
			if got != tc.want {
				t.Errorf("MatchToolError = %q, want %q", got, tc.want)
			}
		})
	}
}
