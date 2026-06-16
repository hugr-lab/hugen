package task

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hugr-lab/hugen/pkg/skill"
)

// describeTaskManifest renders an inline SKILL.md. taskBlock is the
// full `task:` body (may be empty / non-eligible to exercise the
// guard paths).
func describeTaskManifest(name, taskBlock string) string {
	return `---
name: ` + name + `
description: A describable test task.
license: MIT
metadata:
  hugen:` + taskBlock + `
---
Body for ` + name + `.
`
}

// extWithDescribeSkill builds a task Extension backed by an inline
// skill catalogue holding exactly the one manifest.
func extWithDescribeSkill(t *testing.T, name, manifest string) *Extension {
	t.Helper()
	skillStore := skill.NewSkillStore(skill.Options{Inline: map[string][]byte{name: []byte(manifest)}})
	sm := skill.NewSkillManager(skillStore, nil)
	if _, err := sm.Get(context.Background(), name); err != nil {
		t.Fatalf("inline skill %q did not parse/load: %v", name, err)
	}
	return NewExtension(sm, "agt_test", nil)
}

func TestRequiredKeys(t *testing.T) {
	cases := []struct {
		name   string
		schema map[string]any
		want   []string
	}{
		{"nil schema", nil, []string{}},
		{"no required", map[string]any{"type": "object"}, []string{}},
		{"two required", map[string]any{"required": []any{"region", "year"}}, []string{"region", "year"}},
		{"skips non-string + empty", map[string]any{"required": []any{"region", 7, "", "year"}}, []string{"region", "year"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := requiredKeys(tc.schema)
			if got == nil {
				t.Fatalf("requiredKeys returned nil, want non-nil slice")
			}
			if len(got) != len(tc.want) {
				t.Fatalf("keys = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("keys[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestDescribeTaskTool_Shape(t *testing.T) {
	tl := describeTaskTool()
	if tl.Name != "task:describe" {
		t.Errorf("name = %q, want task:describe", tl.Name)
	}
	if tl.Provider != providerName {
		t.Errorf("provider = %q, want %q", tl.Provider, providerName)
	}
	if tl.PermissionObject != PermDescribeTask {
		t.Errorf("perm = %q, want %q", tl.PermissionObject, PermDescribeTask)
	}
	var sch struct {
		Type     string   `json:"type"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(tl.ArgSchema, &sch); err != nil {
		t.Fatalf("ArgSchema is not valid JSON: %v", err)
	}
	if sch.Type != "object" {
		t.Errorf("schema type = %q, want object", sch.Type)
	}
	if len(sch.Required) != 1 || sch.Required[0] != "name" {
		t.Errorf("schema required = %v, want [name]", sch.Required)
	}
}

func TestListEmitsDescribeAndExecute(t *testing.T) {
	e := extWithDescribeSkill(t, "test_task", describeTaskManifest("test_task",
		"\n    task:\n      eligible: true\n      kind: worker"))
	tools, err := e.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var sawDescribe, sawExecute bool
	for _, tl := range tools {
		switch tl.Name {
		case "task:describe":
			sawDescribe = true
		case "task:execute_task":
			sawExecute = true
		}
	}
	if !sawDescribe {
		t.Error("List did not emit task:describe")
	}
	if !sawExecute {
		t.Error("List did not emit task:execute_task")
	}
}

// callDescribe's argument + wiring guards short-circuit BEFORE any
// skill lookup, so they are unit-testable with a nil SkillManager.
func TestCallDescribe_GuardsWithoutSkills(t *testing.T) {
	e := NewExtension(nil, "agt_test", nil)
	cases := []struct {
		name     string
		args     string
		wantCode string
	}{
		{"malformed json", `{bad`, "invalid_args"},
		{"empty name", `{"name":"  "}`, "invalid_args"},
		{"no name key", `{}`, "invalid_args"},
		{"skills not wired", `{"name":"some_task"}`, "no_skill_manager"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := e.callDescribe(context.Background(), json.RawMessage(tc.args))
			if err != nil {
				t.Fatalf("unexpected go error: %v", err)
			}
			if code := errCode(t, raw); code != tc.wantCode {
				t.Errorf("error code = %q, want %q (body %s)", code, tc.wantCode, raw)
			}
		})
	}
}

func TestCallDescribe_NotFound(t *testing.T) {
	e := extWithDescribeSkill(t, "test_task", describeTaskManifest("test_task",
		"\n    task:\n      eligible: true\n      kind: worker"))
	raw, err := e.callDescribe(context.Background(), json.RawMessage(`{"name":"nope"}`))
	if err != nil {
		t.Fatalf("unexpected go error: %v", err)
	}
	if code := errCode(t, raw); code != "task_not_found" {
		t.Errorf("error code = %q, want task_not_found (body %s)", code, raw)
	}
}

func TestCallDescribe_NotEligible(t *testing.T) {
	// A plain skill with no task block — describe must reject it.
	e := extWithDescribeSkill(t, "plain", describeTaskManifest("plain", ""))
	raw, err := e.callDescribe(context.Background(), json.RawMessage(`{"name":"plain"}`))
	if err != nil {
		t.Fatalf("unexpected go error: %v", err)
	}
	if code := errCode(t, raw); code != "not_task_eligible" {
		t.Errorf("error code = %q, want not_task_eligible (body %s)", code, raw)
	}
}

func TestCallDescribe_HappyPath(t *testing.T) {
	manifest := describeTaskManifest("road_report", `
    task:
      eligible: true
      kind: worker
      goal_summary: Summarise roads in a region.
      allowed_tools_default:
        - hugr-main:data-inline_graphql_result
        - python-mcp:run_script
      inputs_schema:
        type: object
        properties:
          region:
            type: string
          year:
            type: integer
        required:
          - region`)
	e := extWithDescribeSkill(t, "road_report", manifest)

	raw, err := e.callDescribe(context.Background(), json.RawMessage(`{"name":"road_report"}`))
	if err != nil {
		t.Fatalf("unexpected go error: %v", err)
	}
	var resp describeResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode describe response: %v (body %s)", err, raw)
	}
	if resp.Name != "road_report" {
		t.Errorf("name = %q, want road_report", resp.Name)
	}
	if resp.Kind != skill.TaskKindWorker {
		t.Errorf("kind = %q, want %q", resp.Kind, skill.TaskKindWorker)
	}
	if resp.GoalSummary != "Summarise roads in a region." {
		t.Errorf("goal_summary = %q", resp.GoalSummary)
	}
	if len(resp.RequiredInputs) != 1 || resp.RequiredInputs[0] != "region" {
		t.Errorf("required_inputs = %v, want [region]", resp.RequiredInputs)
	}
	if len(resp.AllowedToolsDefault) != 2 {
		t.Errorf("allowed_tools_default = %v, want 2 entries", resp.AllowedToolsDefault)
	}
	// inputs_schema BODY must be present (the whole point of describe).
	var sch struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(resp.InputsSchema, &sch); err != nil {
		t.Fatalf("inputs_schema is not valid JSON: %v (raw %s)", err, resp.InputsSchema)
	}
	if _, ok := sch.Properties["region"]; !ok {
		t.Errorf("inputs_schema missing region property: %s", resp.InputsSchema)
	}
}
