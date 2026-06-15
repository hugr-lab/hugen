package template

import (
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

func TestNewFireRenderContext_NilFireContext(t *testing.T) {
	ctx := NewFireRenderContext(nil)
	if ctx.Inputs == nil {
		t.Fatal("Inputs must be non-nil (empty map) for nil FireContext")
	}
	if len(ctx.Inputs) != 0 {
		t.Errorf("Inputs len=%d, want 0", len(ctx.Inputs))
	}
	if ctx.FireTime.IsZero() {
		t.Error("FireTime should default to now, got zero")
	}
}

func TestNewFireRenderContext_ZeroInputsMap(t *testing.T) {
	fc := &protocol.FireContext{
		TaskID:    "tsk_test",
		FireSeq:   1,
		PlannedAt: time.Now(),
		Goal:      "do the thing",
		Inputs:    nil,
	}
	ctx := NewFireRenderContext(fc)
	if ctx.Inputs == nil {
		t.Fatal("Inputs must be non-nil even when source map is nil")
	}
	if ctx.Goal != "do the thing" {
		t.Errorf("Goal=%q, want %q", ctx.Goal, "do the thing")
	}
}

func TestRenderTemplate_GoalAndInputs(t *testing.T) {
	fc := &protocol.FireContext{
		TaskID:    "tsk_aa",
		FireSeq:   2,
		PlannedAt: time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC),
		Goal:      "summarise the dashboard for {{ .Inputs.region }}",
		Inputs: map[string]any{
			"region":   "EU",
			"language": "en",
		},
	}
	ctx := NewFireRenderContext(fc)
	got, err := RenderTemplate(`Fire #{{ .FireSeq }} of task {{ .TaskID }} — `+
		`region={{ .Inputs.region }}, lang={{ .Inputs.language }}`, ctx)
	if err != nil {
		t.Fatalf("RenderTemplate: %v", err)
	}
	want := "Fire #2 of task tsk_aa — region=EU, lang=en"
	if got != want {
		t.Errorf("got=%q, want=%q", got, want)
	}
}

func TestRenderInputs_PerFireSubstitution(t *testing.T) {
	ctx := NewFireRenderContext(&protocol.FireContext{
		TaskID:  "tsk_z",
		FireSeq: 3,
	})
	in := map[string]any{
		"output_path": "report_{{ .FireSeq }}.html",
		"literal":     "<time>", // naive placeholder — stays literal
		"count":       7,        // non-string passes through
		"nested": map[string]any{
			"file": "n_{{ .FireSeq }}.csv",
		},
		"list": []any{"a_{{ .FireSeq }}", "plain"},
	}
	out, err := RenderInputs(in, ctx)
	if err != nil {
		t.Fatalf("RenderInputs: %v", err)
	}
	if out["output_path"] != "report_3.html" {
		t.Errorf("output_path = %v, want report_3.html", out["output_path"])
	}
	if out["literal"] != "<time>" {
		t.Errorf("literal = %v, want <time> (unrendered)", out["literal"])
	}
	if out["count"] != 7 {
		t.Errorf("count = %v, want 7", out["count"])
	}
	nested, _ := out["nested"].(map[string]any)
	if nested["file"] != "n_3.csv" {
		t.Errorf("nested.file = %v, want n_3.csv", nested["file"])
	}
	list, _ := out["list"].([]any)
	if len(list) != 2 || list[0] != "a_3" || list[1] != "plain" {
		t.Errorf("list = %v, want [a_3 plain]", list)
	}
	// Input map is NOT mutated.
	if in["output_path"] != "report_{{ .FireSeq }}.html" {
		t.Errorf("source map mutated: %v", in["output_path"])
	}
}

func TestRenderInputs_BadTemplateReportsKey(t *testing.T) {
	ctx := NewFireRenderContext(nil)
	_, err := RenderInputs(map[string]any{
		"good": "fine",
		"bad":  "{{ .Nope ",
	}, ctx)
	if err == nil {
		t.Fatal("expected error for malformed input template")
	}
	if !strings.Contains(err.Error(), "inputs.bad") {
		t.Errorf("error should name the offending key: %v", err)
	}
}

func TestRenderInputs_EmptyPassThrough(t *testing.T) {
	ctx := NewFireRenderContext(nil)
	out, err := RenderInputs(nil, ctx)
	if err != nil {
		t.Fatalf("RenderInputs(nil): %v", err)
	}
	if out != nil {
		t.Errorf("nil inputs should pass through as nil, got %v", out)
	}
}

// TestRenderInputs_EmptyMapNotAliased proves a non-nil empty input map
// yields a FRESH map, not the caller's reference — so mutating the
// result can never corrupt the stored task spec.
func TestRenderInputs_EmptyMapNotAliased(t *testing.T) {
	ctx := NewFireRenderContext(nil)
	in := map[string]any{}
	out, err := RenderInputs(in, ctx)
	if err != nil {
		t.Fatalf("RenderInputs(empty): %v", err)
	}
	out["injected"] = 1
	if len(in) != 0 {
		t.Errorf("source map was mutated through the returned map: %v", in)
	}
}

func TestRenderTemplate_PrevFireGuard(t *testing.T) {
	tmpl := `{{ if .PrevFire }}prior: {{ .PrevFire.Summary }}{{ else }}first run{{ end }}`

	first, err := RenderTemplate(tmpl, NewFireRenderContext(&protocol.FireContext{
		TaskID:  "tsk_pf",
		FireSeq: 1,
	}))
	if err != nil {
		t.Fatalf("first render: %v", err)
	}
	if first != "first run" {
		t.Errorf("first run got=%q", first)
	}

	withPrev, err := RenderTemplate(tmpl, NewFireRenderContext(&protocol.FireContext{
		TaskID:  "tsk_pf",
		FireSeq: 2,
		PrevFire: &protocol.PrevFireOutcome{
			Summary: "reaped 3 alerts",
		},
	}))
	if err != nil {
		t.Fatalf("withPrev render: %v", err)
	}
	if !strings.Contains(withPrev, "reaped 3 alerts") {
		t.Errorf("withPrev render missing summary: %q", withPrev)
	}
}

func TestFuncMap_FormatDate(t *testing.T) {
	planned := time.Date(2026, 6, 1, 13, 45, 0, 0, time.UTC)
	fc := &protocol.FireContext{TaskID: "tsk_fd", PlannedAt: planned}
	ctx := NewFireRenderContext(fc)
	out, err := RenderTemplate(`{{ formatDate "2006-01-02 15:04" .PlannedAt }}`, ctx)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if out != "2026-06-01 13:45" {
		t.Errorf("formatDate got=%q", out)
	}
}

func TestFuncMap_FormatDate_ZeroIsEmpty(t *testing.T) {
	fc := &protocol.FireContext{TaskID: "tsk_fd_zero"}
	ctx := NewFireRenderContext(fc)
	// FireTime is set to now in NewFireRenderContext, but PlannedAt
	// remains zero on a wake-only task.
	out, err := RenderTemplate(`x={{ formatDate "2006-01-02" .PlannedAt }}.`, ctx)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if out != "x=." {
		t.Errorf("zero PlannedAt should render empty, got %q", out)
	}
}

func TestFuncMap_AddDuration(t *testing.T) {
	planned := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	fc := &protocol.FireContext{TaskID: "tsk_dd", PlannedAt: planned}
	ctx := NewFireRenderContext(fc)
	out, err := RenderTemplate(
		`{{ formatDate "15:04" (addDuration .PlannedAt "2h30m") }}`, ctx)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if out != "11:30" {
		t.Errorf("addDuration got=%q", out)
	}
}

func TestFuncMap_AddDuration_InvalidSpecErrors(t *testing.T) {
	ctx := NewFireRenderContext(&protocol.FireContext{
		TaskID: "tsk_dd", PlannedAt: time.Now(),
	})
	_, err := RenderTemplate(
		`{{ formatDate "15:04" (addDuration .PlannedAt "not-a-duration") }}`, ctx)
	if err == nil {
		t.Fatal("expected error on invalid duration spec, got nil")
	}
}

func TestFuncMap_Default_MissingInput(t *testing.T) {
	fc := &protocol.FireContext{
		TaskID:  "tsk_def",
		FireSeq: 1,
		Inputs: map[string]any{
			"region": "EU",
			// language intentionally absent
		},
	}
	ctx := NewFireRenderContext(fc)
	out, err := RenderTemplate(
		`lang={{ default "en" (index .Inputs "language") }}`, ctx)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if out != "lang=en" {
		t.Errorf("default got=%q", out)
	}
}

func TestRenderTemplate_ParseErrorWrapped(t *testing.T) {
	_, err := RenderTemplate(`{{ .Goal `, NewFireRenderContext(nil))
	if err == nil {
		t.Fatal("expected parse error")
	}
	if !strings.Contains(err.Error(), "template parse") {
		t.Errorf("err=%q, want wrap with 'template parse'", err)
	}
}

func TestRenderInto_NilTemplateErrors(t *testing.T) {
	_, err := RenderInto(nil, NewFireRenderContext(nil))
	if err == nil {
		t.Fatal("expected nil-template error")
	}
}
