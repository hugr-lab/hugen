package mission

import (
	"strings"
	"testing"
)

func TestRenderInlinePlan_StaticFieldsPassThrough(t *testing.T) {
	plan := &InlinePlan{Waves: []Wave{{
		Label: "run",
		Subagents: []SubagentSpec{{
			Name:  "runner",
			Skill: "static-skill",
			Task:  "do the thing",
			Inputs: map[string]any{
				"region": "EU",
				"limit":  10,
			},
		}},
	}}}

	out, err := renderInlinePlan(plan, map[string]any{"unused": "x"}, nil)
	if err != nil {
		t.Fatalf("renderInlinePlan: %v", err)
	}
	if len(out.Waves) != 1 || len(out.Waves[0].Subagents) != 1 {
		t.Fatalf("shape mismatch: %+v", out)
	}
	sub := out.Waves[0].Subagents[0]
	if sub.Skill != "static-skill" || sub.Task != "do the thing" {
		t.Errorf("static fields mutated: skill=%q task=%q", sub.Skill, sub.Task)
	}
	im, ok := sub.Inputs.(map[string]any)
	if !ok {
		t.Fatalf("inputs lost map shape: %T", sub.Inputs)
	}
	if im["region"] != "EU" || im["limit"] != 10 {
		t.Errorf("inputs map mutated: %+v", im)
	}
}

func TestRenderInlinePlan_SkillAndTaskTemplated(t *testing.T) {
	plan := &InlinePlan{Waves: []Wave{{
		Label: "run",
		Subagents: []SubagentSpec{{
			Name:  "runner",
			Skill: "{{ .Inputs.task_skill }}",
			Task:  "execute {{ .Inputs.task_brief }}",
		}},
	}}}
	spawn := map[string]any{
		"task_skill": "daily_report",
		"task_brief": "summary for today",
	}
	out, err := renderInlinePlan(plan, spawn, nil)
	if err != nil {
		t.Fatalf("renderInlinePlan: %v", err)
	}
	sub := out.Waves[0].Subagents[0]
	if sub.Skill != "daily_report" {
		t.Errorf("Skill render: got %q want %q", sub.Skill, "daily_report")
	}
	if sub.Task != "execute summary for today" {
		t.Errorf("Task render: got %q", sub.Task)
	}
}

func TestRenderInlinePlan_ResolvedOverridesSpawnInMerge(t *testing.T) {
	plan := &InlinePlan{Waves: []Wave{{
		Label: "run",
		Subagents: []SubagentSpec{{
			Name: "runner",
			Task: "{{ .Inputs.region }} / {{ .Inputs.lang }}",
		}},
	}}}
	spawn := map[string]any{"region": "default", "lang": "en"}
	resolved := map[string]any{"region": "EU"} // research overrides region
	out, err := renderInlinePlan(plan, spawn, resolved)
	if err != nil {
		t.Fatalf("renderInlinePlan: %v", err)
	}
	got := out.Waves[0].Subagents[0].Task
	if got != "EU / en" {
		t.Errorf("merge precedence: got %q want %q", got, "EU / en")
	}
}

func TestRenderInlinePlan_MissingKeyRendersEmpty(t *testing.T) {
	plan := &InlinePlan{Waves: []Wave{{
		Label: "run",
		Subagents: []SubagentSpec{{
			Name: "runner",
			Task: "x={{ .Inputs.absent }}.",
		}},
	}}}
	out, err := renderInlinePlan(plan, nil, nil)
	if err != nil {
		t.Fatalf("renderInlinePlan: %v", err)
	}
	if got := out.Waves[0].Subagents[0].Task; got != "x=." {
		t.Errorf("missingkey=zero: got %q", got)
	}
}

func TestRenderInlinePlan_InputsMapStringLeavesRendered(t *testing.T) {
	plan := &InlinePlan{Waves: []Wave{{
		Label: "run",
		Subagents: []SubagentSpec{{
			Name: "runner",
			Inputs: map[string]any{
				"region":  "{{ .Inputs.region }}",
				"limit":   10, // non-string preserved verbatim
				"nested":  map[string]any{"key": "{{ .Inputs.region }}-suffix"},
				"choices": []any{"{{ .Inputs.region }}", "static"},
			},
		}},
	}}}
	spawn := map[string]any{"region": "EU"}
	out, err := renderInlinePlan(plan, spawn, nil)
	if err != nil {
		t.Fatalf("renderInlinePlan: %v", err)
	}
	im := out.Waves[0].Subagents[0].Inputs.(map[string]any)
	if im["region"] != "EU" {
		t.Errorf("string leaf: got %v", im["region"])
	}
	if im["limit"] != 10 {
		t.Errorf("int leaf preserved: got %v", im["limit"])
	}
	nested := im["nested"].(map[string]any)
	if nested["key"] != "EU-suffix" {
		t.Errorf("nested map leaf: got %v", nested["key"])
	}
	choices := im["choices"].([]any)
	if choices[0] != "EU" || choices[1] != "static" {
		t.Errorf("slice leaves: got %v", choices)
	}
}

func TestRenderInlinePlan_InputsFromResolvedReplacesMap(t *testing.T) {
	plan := &InlinePlan{Waves: []Wave{{
		Label: "run",
		Subagents: []SubagentSpec{{
			Name:               "runner",
			Skill:              "recipe",
			InputsFromResolved: true,
		}},
	}}}
	resolved := map[string]any{
		"file_path": "~/foo.html",
		"region":    "EU",
	}
	out, err := renderInlinePlan(plan, nil, resolved)
	if err != nil {
		t.Fatalf("renderInlinePlan: %v", err)
	}
	got := out.Waves[0].Subagents[0].Inputs.(map[string]any)
	if got["file_path"] != "~/foo.html" || got["region"] != "EU" {
		t.Errorf("InputsFromResolved: got %+v", got)
	}
	// Mutating the rendered map must not affect resolved.
	got["region"] = "MUTATED"
	if resolved["region"] != "EU" {
		t.Errorf("rendered inputs share storage with resolved map")
	}
}

func TestRenderInlinePlan_InputsFromResolvedWithLiteralRejected(t *testing.T) {
	plan := &InlinePlan{Waves: []Wave{{
		Label: "run",
		Subagents: []SubagentSpec{{
			Name:               "runner",
			InputsFromResolved: true,
			Inputs:             map[string]any{"x": 1},
		}},
	}}}
	_, err := renderInlinePlan(plan, nil, nil)
	if err == nil {
		t.Fatal("expected error on InputsFromResolved + literal Inputs")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("err=%q, want mention of mutual exclusion", err)
	}
}

func TestRenderInlinePlan_TemplateParseErrorBubbles(t *testing.T) {
	plan := &InlinePlan{Waves: []Wave{{
		Label: "run",
		Subagents: []SubagentSpec{{
			Name:  "runner",
			Skill: "{{ .Inputs.task_skill",
		}},
	}}}
	_, err := renderInlinePlan(plan, nil, nil)
	if err == nil {
		t.Fatal("expected parse error")
	}
	if !strings.Contains(err.Error(), "skill") {
		t.Errorf("err=%q, want context 'skill'", err)
	}
}

func TestRenderInlinePlan_NilPlanReturnsNil(t *testing.T) {
	out, err := renderInlinePlan(nil, nil, nil)
	if err != nil || out != nil {
		t.Errorf("nil plan: out=%v err=%v", out, err)
	}
}

func TestRenderInlinePlan_WavePropertiesPreserved(t *testing.T) {
	plan := &InlinePlan{Waves: []Wave{{
		Label:              "research-wave",
		SkipCheck:          true,
		AcceptanceCriteria: []string{"AC-1: result is sane"},
		Subagents:          []SubagentSpec{{Name: "r"}},
	}}}
	out, err := renderInlinePlan(plan, nil, nil)
	if err != nil {
		t.Fatalf("renderInlinePlan: %v", err)
	}
	w := out.Waves[0]
	if w.Label != "research-wave" {
		t.Errorf("label lost: %q", w.Label)
	}
	if !w.SkipCheck {
		t.Errorf("SkipCheck lost")
	}
	if len(w.AcceptanceCriteria) != 1 || w.AcceptanceCriteria[0] != "AC-1: result is sane" {
		t.Errorf("AC lost: %+v", w.AcceptanceCriteria)
	}
	// And the AC slice must be a copy, not aliased.
	w.AcceptanceCriteria[0] = "MUTATED"
	if plan.Waves[0].AcceptanceCriteria[0] == "MUTATED" {
		t.Errorf("AC slice aliased with input plan")
	}
}

func TestRenderInlinePlan_DependsOnCopied(t *testing.T) {
	plan := &InlinePlan{Waves: []Wave{{
		Label: "run",
		Subagents: []SubagentSpec{{
			Name:      "runner",
			DependsOn: []string{"input-collector@research"},
		}},
	}}}
	out, err := renderInlinePlan(plan, nil, nil)
	if err != nil {
		t.Fatalf("renderInlinePlan: %v", err)
	}
	dep := out.Waves[0].Subagents[0].DependsOn
	if len(dep) != 1 || dep[0] != "input-collector@research" {
		t.Errorf("DependsOn lost: %+v", dep)
	}
	dep[0] = "MUTATED"
	if plan.Waves[0].Subagents[0].DependsOn[0] == "MUTATED" {
		t.Errorf("DependsOn slice aliased with input plan")
	}
}

func TestMergeInputs_BothEmptyReturnsNil(t *testing.T) {
	if got := mergeInputs(nil, nil); got != nil {
		t.Errorf("nil+nil: got %v", got)
	}
	if got := mergeInputs(map[string]any{}, map[string]any{}); got != nil {
		t.Errorf("empty+empty: got %v", got)
	}
}

func TestMergeInputs_ResolvedWins(t *testing.T) {
	out := mergeInputs(
		map[string]any{"a": 1, "b": 2},
		map[string]any{"b": 99, "c": 3},
	)
	want := map[string]any{"a": 1, "b": 99, "c": 3}
	for k, v := range want {
		if out[k] != v {
			t.Errorf("key %q: got %v want %v", k, out[k], v)
		}
	}
}
