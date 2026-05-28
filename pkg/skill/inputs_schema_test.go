package skill

import (
	"strings"
	"testing"
)

func TestRenderInputsSchemaBlock_RequiredAndOptional(t *testing.T) {
	schema := map[string]any{
		"type":     "object",
		"required": []any{"task_skill"},
		"properties": map[string]any{
			"task_skill": map[string]any{
				"type":        "string",
				"description": "Recipe name from Available tasks block",
			},
			"task_inputs": map[string]any{
				"type":        "object",
				"description": "Pre-filled inputs matching the recipe's schema",
			},
		},
	}
	out := RenderInputsSchemaBlock(schema, "  ")
	if !strings.Contains(out, "  inputs (required):\n    task_skill (string) — Recipe name from Available tasks block") {
		t.Errorf("required block missing or malformed:\n%s", out)
	}
	if !strings.Contains(out, "  inputs (optional):\n    task_inputs (object) — Pre-filled inputs matching the recipe's schema") {
		t.Errorf("optional block missing or malformed:\n%s", out)
	}
	// Required block must precede optional block.
	if strings.Index(out, "required") > strings.Index(out, "optional") {
		t.Errorf("required block must precede optional:\n%s", out)
	}
}

func TestRenderInputsSchemaBlock_RequiredOnly(t *testing.T) {
	schema := map[string]any{
		"required": []any{"x"},
		"properties": map[string]any{
			"x": map[string]any{"type": "string"},
		},
	}
	out := RenderInputsSchemaBlock(schema, "  ")
	if !strings.Contains(out, "inputs (required):") {
		t.Errorf("required block missing:\n%s", out)
	}
	if strings.Contains(out, "optional") {
		t.Errorf("optional block must be absent when no optional fields:\n%s", out)
	}
}

func TestRenderInputsSchemaBlock_OptionalOnly(t *testing.T) {
	schema := map[string]any{
		"properties": map[string]any{
			"y": map[string]any{"type": "integer", "description": "row count"},
		},
	}
	out := RenderInputsSchemaBlock(schema, "  ")
	if strings.Contains(out, "required") {
		t.Errorf("required block must be absent when no required fields:\n%s", out)
	}
	if !strings.Contains(out, "y (integer) — row count") {
		t.Errorf("entry missing:\n%s", out)
	}
}

func TestRenderInputsSchemaBlock_NoTypeNoDesc(t *testing.T) {
	schema := map[string]any{
		"properties": map[string]any{
			"raw": map[string]any{}, // empty property block
		},
	}
	out := RenderInputsSchemaBlock(schema, "  ")
	// Bare name only; no `(…)`, no `— …`.
	if !strings.Contains(out, "    raw\n") && !strings.HasSuffix(out, "    raw") {
		t.Errorf("bare name not rendered cleanly:\n%s", out)
	}
}

func TestRenderInputsSchemaBlock_NilOrEmptyReturnsEmpty(t *testing.T) {
	if got := RenderInputsSchemaBlock(nil, ""); got != "" {
		t.Errorf("nil schema: got %q", got)
	}
	if got := RenderInputsSchemaBlock(map[string]any{}, ""); got != "" {
		t.Errorf("empty schema: got %q", got)
	}
	if got := RenderInputsSchemaBlock(map[string]any{"type": "object"}, ""); got != "" {
		t.Errorf("schema with no properties: got %q", got)
	}
	if got := RenderInputsSchemaBlock(map[string]any{"properties": map[string]any{}}, ""); got != "" {
		t.Errorf("empty properties: got %q", got)
	}
}

func TestRenderInputsSchemaBlock_RequiredStringSliceAlsoAccepted(t *testing.T) {
	schema := map[string]any{
		// Some YAML decoders produce []string, not []any.
		"required": []string{"a"},
		"properties": map[string]any{
			"a": map[string]any{"type": "string"},
			"b": map[string]any{"type": "string"},
		},
	}
	out := RenderInputsSchemaBlock(schema, "  ")
	if !strings.Contains(out, "inputs (required):\n    a (string)") {
		t.Errorf("required (from []string): missing:\n%s", out)
	}
	if !strings.Contains(out, "inputs (optional):\n    b (string)") {
		t.Errorf("optional missing:\n%s", out)
	}
}

func TestRenderInputsSchemaBlock_MultilineDescriptionCollapsed(t *testing.T) {
	schema := map[string]any{
		"required": []any{"k"},
		"properties": map[string]any{
			"k": map[string]any{
				"type":        "string",
				"description": "line one\n  line two\n\nline three",
			},
		},
	}
	out := RenderInputsSchemaBlock(schema, "")
	if strings.Contains(out, "\n  ") {
		// Acceptable indentation prefix is the inner-indent prefix
		// the helper added; description content must not carry
		// internal newlines.
		if strings.Count(out, "\n") > 2 {
			t.Errorf("description newlines not collapsed:\n%q", out)
		}
	}
	if !strings.Contains(out, "line one line two line three") {
		t.Errorf("collapsed description missing:\n%s", out)
	}
}

func TestRenderInputsSchemaBlock_SortsAlphabetically(t *testing.T) {
	schema := map[string]any{
		"properties": map[string]any{
			"zeta":  map[string]any{"type": "string"},
			"alpha": map[string]any{"type": "string"},
			"mu":    map[string]any{"type": "string"},
		},
	}
	out := RenderInputsSchemaBlock(schema, "")
	idxAlpha := strings.Index(out, "alpha")
	idxMu := strings.Index(out, "mu")
	idxZeta := strings.Index(out, "zeta")
	if !(idxAlpha < idxMu && idxMu < idxZeta) {
		t.Errorf("alphabetical sort lost (alpha=%d mu=%d zeta=%d):\n%s",
			idxAlpha, idxMu, idxZeta, out)
	}
}
