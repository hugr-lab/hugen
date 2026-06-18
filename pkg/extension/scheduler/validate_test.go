package scheduler

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	schedstore "github.com/hugr-lab/hugen/pkg/scheduler/store"
	"github.com/hugr-lab/hugen/pkg/skill"
)

func TestMissingRequiredInputs(t *testing.T) {
	schema := map[string]any{
		"type":     "object",
		"required": []any{"region", "year"},
	}
	cases := []struct {
		name   string
		inputs map[string]any
		want   []string
	}{
		{"all present", map[string]any{"region": "EU", "year": 2025}, nil},
		{"one missing", map[string]any{"region": "EU"}, []string{"year"}},
		{"nil inputs → all missing", nil, []string{"region", "year"}},
		{"explicit null counts missing", map[string]any{"region": "EU", "year": nil}, []string{"year"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := missingRequiredInputs(schema, tc.inputs)
			if len(got) != len(tc.want) {
				t.Fatalf("missing = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("missing[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
	// No `required` array / no schema → nothing mandatory.
	if got := missingRequiredInputs(map[string]any{"type": "object"}, nil); got != nil {
		t.Errorf("no required array: got %v, want nil", got)
	}
	if got := missingRequiredInputs(nil, nil); got != nil {
		t.Errorf("nil schema: got %v, want nil", got)
	}
}

// taskSkillManifest renders an inline task-eligible SKILL.md. requiredYAML
// is the inputs_schema `required` block (may be empty); goalSummary may be
// empty to exercise the missing-goal path.
func taskSkillManifest(goalSummary, requiredBlock string) string {
	gs := ""
	if goalSummary != "" {
		gs = "\n      goal_summary: " + goalSummary
	}
	return `---
name: test_task
description: A scheduled test task.
license: MIT
metadata:
  hugen:
    task:
      eligible: true
      kind: worker` + gs + `
      allowed_tools_default:
        - hugr-main:data-inline_graphql_result
        - python-mcp:run_script
      inputs_schema:
        type: object
        properties:
          region:
            type: string
` + requiredBlock + `
---
Run the report for {{ .Inputs.region }}.
`
}

func extWithTaskSkill(t *testing.T, manifest string) (*Extension, *fakeStore) {
	t.Helper()
	store := newFakeStore()
	skillStore := skill.NewSkillStore(skill.Options{Inline: map[string][]byte{"test_task": []byte(manifest)}})
	sm := skill.NewSkillManager(skillStore, nil)
	// Guard: a malformed manifest is silently dropped by the inline
	// backend, which would mask the test behind an unknown_skill error.
	if _, err := sm.Get(context.Background(), "test_task"); err != nil {
		t.Fatalf("inline task skill did not parse/load: %v", err)
	}
	return NewExtension(store, sm, "agt_test", nil), store
}

func spawnCreateArgs(extra map[string]any) map[string]any {
	args := map[string]any{
		"kind":               schedstore.KindSpawn,
		"skill_ref":          "test_task",
		"schedule_kind":      schedstore.ScheduleInterval,
		"schedule_spec":      "1h",
		"initial_planned_at": time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano),
		"name":               "scheduled test",
		"end_condition":      map[string]any{"kind": "until_cancel"},
	}
	for k, v := range extra {
		args[k] = v
	}
	return args
}

func createErrCode(t *testing.T, body json.RawMessage) string {
	t.Helper()
	var resp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode create error: %v (body %s)", err, body)
	}
	return resp.Error.Code
}

func TestCallCreate_RejectsNonSchedulableTask(t *testing.T) {
	// A task with disable_scheduling: true (an interactive task that
	// prompts the user) cannot be bound to a headless schedule.
	const manifest = `---
name: test_task
description: An interactive task that prompts the user.
license: MIT
metadata:
  hugen:
    task:
      eligible: true
      kind: worker
      disable_scheduling: true
      goal_summary: do the interactive thing
---
body
`
	ext, _ := extWithTaskSkill(t, manifest)
	owner := newFakeState("ses-owner-nosched")
	body := callTool(t, ext, owner, "create", spawnCreateArgs(nil))
	if code := createErrCode(t, body); code != "not_schedulable" {
		t.Errorf("error code = %q, want not_schedulable (body %s)", code, body)
	}
}

func TestCallCreate_RejectsMissingRequiredInputs(t *testing.T) {
	ext, _ := extWithTaskSkill(t, taskSkillManifest("Generate the report", "        required:\n          - region"))
	owner := newFakeState("ses-owner")
	// No `inputs` → the required `region` is unsatisfiable headless.
	body := callTool(t, ext, owner, "create", spawnCreateArgs(nil))
	if code := createErrCode(t, body); code != "missing_inputs" {
		t.Errorf("error code = %q, want missing_inputs (body %s)", code, body)
	}
}

func TestCallCreate_RejectsMissingGoal(t *testing.T) {
	// No goal_summary in the manifest, no `required` inputs, and no
	// explicit goal on the call → unfireable (empty kick body).
	ext, _ := extWithTaskSkill(t, taskSkillManifest("", ""))
	owner := newFakeState("ses-owner")
	body := callTool(t, ext, owner, "create", spawnCreateArgs(map[string]any{
		"inputs": map[string]any{"region": "EU"},
	}))
	if code := createErrCode(t, body); code != "missing_goal" {
		t.Errorf("error code = %q, want missing_goal (body %s)", code, body)
	}
}

func TestCallCreate_FreezesGoalFromSummary(t *testing.T) {
	// goal_summary present, no explicit goal → create succeeds and the
	// persisted row freezes the goal from the summary so each fire has a
	// kick body.
	ext, store := extWithTaskSkill(t, taskSkillManifest("Generate the road report", ""))
	owner := newFakeState("ses-owner")
	body := callTool(t, ext, owner, "create", spawnCreateArgs(map[string]any{
		"inputs": map[string]any{"region": "EU"},
	}))
	var out createOutput
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode create output: %v (body %s)", err, body)
	}
	if out.TaskID == "" {
		t.Fatalf("expected a created task; got error body %s", body)
	}
	row, err := store.GetTask(context.Background(), out.TaskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if row.Spec.Goal != "Generate the road report" {
		t.Errorf("frozen goal = %q, want the goal_summary", row.Spec.Goal)
	}
	// allowed_tools frozen from the manifest's allowed_tools_default
	// (the caller passed none) so the headless fire has a tool budget.
	if len(row.Spec.AllowedTools) != 2 ||
		row.Spec.AllowedTools[0] != "hugr-main:data-inline_graphql_result" ||
		row.Spec.AllowedTools[1] != "python-mcp:run_script" {
		t.Errorf("frozen allowed_tools = %v, want the manifest default", row.Spec.AllowedTools)
	}
}

func TestCallCreate_ExplicitGoalWinsOverSummary(t *testing.T) {
	ext, store := extWithTaskSkill(t, taskSkillManifest("summary goal", ""))
	owner := newFakeState("ses-owner")
	body := callTool(t, ext, owner, "create", spawnCreateArgs(map[string]any{
		"inputs": map[string]any{"region": "EU"},
		"goal":   "explicit goal",
	}))
	var out createOutput
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v (body %s)", err, body)
	}
	if out.TaskID == "" {
		t.Fatalf("expected success; got %s", body)
	}
	row, _ := store.GetTask(context.Background(), out.TaskID)
	if row.Spec.Goal != "explicit goal" {
		t.Errorf("goal = %q, want explicit goal", row.Spec.Goal)
	}
}
