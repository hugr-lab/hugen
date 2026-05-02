package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateLLMSchema_Accepts(t *testing.T) {
	good := []string{
		`{"type":"object","properties":{"x":{"type":"string"}},"required":["x"]}`,
		`{"type":"object","properties":{"a":{"type":"array","items":{"type":"string"}}}}`,
		`{"type":"object","properties":{"q":{"type":"object","properties":{"y":{"type":"number"}}}}}`,
		`{"type":"object"}`,
		``,
	}
	for _, s := range good {
		if err := ValidateLLMSchema(json.RawMessage(s)); err != nil {
			t.Errorf("expected accept for %q: %v", s, err)
		}
	}
}

func TestValidateLLMSchema_Rejects(t *testing.T) {
	cases := []struct {
		name   string
		schema string
		needle string
	}{
		{"non-object top", `{"type":"string"}`, "type must be"},
		{"array no items", `{"type":"object","properties":{"a":{"type":"array"}}}`, "items"},
		{"additionalProperties root", `{"type":"object","additionalProperties":true}`, "additionalProperties"},
		{"additionalProperties nested", `{"type":"object","properties":{"e":{"type":"object","additionalProperties":{"type":"string"}}}}`, "additionalProperties"},
		{"$ref", `{"type":"object","properties":{"x":{"$ref":"#/defs/X"}}}`, "$ref"},
		{"oneOf", `{"type":"object","oneOf":[{"type":"object"}]}`, "oneOf"},
		{"anyOf nested", `{"type":"object","properties":{"x":{"anyOf":[{"type":"string"}]}}}`, "anyOf"},
		{"allOf", `{"type":"object","allOf":[{"type":"object"}]}`, "allOf"},
		{"items array w/o nested items", `{"type":"object","properties":{"a":{"type":"array","items":{"type":"array"}}}}`, "items"},
		{"missing top type", `{"properties":{}}`, "type must be"},
		{"invalid json", `{`, "invalid JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateLLMSchema(json.RawMessage(tc.schema))
			if err == nil {
				t.Fatalf("expected error for %s", tc.schema)
			}
			if !strings.Contains(err.Error(), tc.needle) {
				t.Errorf("error %q does not contain %q", err, tc.needle)
			}
		})
	}
}

func TestSanitizeLLMSchema(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		repairs int
	}{
		{
			name:    "drop additionalProperties",
			in:      `{"type":"object","properties":{"e":{"type":"object","additionalProperties":{"type":"string"}}}}`,
			want:    `{"properties":{"e":{"type":"object"}},"type":"object"}`,
			repairs: 1,
		},
		{
			name:    "inject items into array",
			in:      `{"type":"object","properties":{"a":{"type":"array"}}}`,
			want:    `{"properties":{"a":{"items":{},"type":"array"}},"type":"object"}`,
			repairs: 1,
		},
		{
			name:    "drop $ref",
			in:      `{"type":"object","properties":{"x":{"$ref":"#/X"}}}`,
			want:    `{"properties":{"x":{}},"type":"object"}`,
			repairs: 1,
		},
		{
			name:    "force top-level type",
			in:      `{"properties":{"x":{"type":"string"}}}`,
			want:    `{"properties":{"x":{"type":"string"}},"type":"object"}`,
			repairs: 1,
		},
		{
			name:    "untouched when clean",
			in:      `{"type":"object","properties":{"x":{"type":"string"}}}`,
			want:    `{"type":"object","properties":{"x":{"type":"string"}}}`,
			repairs: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, repairs, err := SanitizeLLMSchema(json.RawMessage(tc.in))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(repairs) != tc.repairs {
				t.Errorf("want %d repair notes, got %d (%v)", tc.repairs, len(repairs), repairs)
			}
			// Re-validate the output: must always pass.
			if err := ValidateLLMSchema(out); err != nil {
				t.Errorf("sanitised output still invalid: %v\nout: %s", err, out)
			}
			if string(out) != tc.want {
				t.Errorf("output mismatch:\nwant %s\ngot  %s", tc.want, out)
			}
		})
	}
}

// Asserts every static schema baked into SystemProvider passes the
// validator. SystemProvider.List itself is fail-fast, so this gives
// a clean error message on regression rather than runtime panics.
func TestSystemProvider_AllSchemasValid(t *testing.T) {
	prov := NewSystemProvider(SystemDeps{AgentID: "agt-test"})
	tools, err := prov.List(context.Background())
	if err != nil {
		t.Fatalf("SystemProvider.List: %v", err)
	}
	if len(tools) == 0 {
		t.Fatal("SystemProvider.List returned no tools")
	}
	for _, tl := range tools {
		if err := ValidateLLMSchema(tl.ArgSchema); err != nil {
			t.Errorf("tool %q schema invalid: %v\nschema: %s", tl.Name, err, tl.ArgSchema)
		}
	}
}
