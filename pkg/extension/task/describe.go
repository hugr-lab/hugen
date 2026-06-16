package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hugr-lab/hugen/pkg/skill"
)

// describeResponse is the body of a successful `task:describe`. It
// surfaces the task contract a caller needs to collect inputs + launch
// — without loading the (worker-tier) skill, which root cannot do.
type describeResponse struct {
	Name                string          `json:"name"`
	Description         string          `json:"description,omitempty"`
	GoalSummary         string          `json:"goal_summary,omitempty"`
	Kind                string          `json:"kind"`
	InputsSchema        json.RawMessage `json:"inputs_schema,omitempty"`
	RequiredInputs      []string        `json:"required_inputs"`
	AllowedToolsDefault []string        `json:"allowed_tools_default,omitempty"`
}

// callDescribe implements `task:describe(name)` — the read tool that
// reveals a task's inputs_schema (required + optional params, types,
// defaults) + goal_summary, so the model knows what inputs to collect
// from the user BEFORE running or scheduling the task. It is the only
// surface that returns the inputs_schema BODY: `skill:catalog_list`
// returns just name + description + a has-inputs-schema flag, and a
// task-eligible skill is worker-tier so root cannot `skill:load` it to
// read the body (tier_forbidden). Pure read — no spawn, no approval.
func (e *Extension) callDescribe(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	var in struct {
		Name string `json:"name"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return toolErr("invalid_args",
				fmt.Sprintf("task:describe args is not valid JSON: %v", err)), nil
		}
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return toolErr("invalid_args", "task:describe requires a non-empty task name"), nil
	}
	if e.skills == nil {
		return toolErr("no_skill_manager", "skill manager not wired"), nil
	}

	sk, err := e.skills.Get(ctx, name)
	if err != nil {
		if errors.Is(err, skill.ErrSkillNotFound) {
			return toolErr("task_not_found",
				fmt.Sprintf("no task named %q — search with task:search", name)), nil
		}
		return nil, fmt.Errorf("task:describe: skill lookup %q: %w", name, err)
	}
	tb := sk.Manifest.Hugen.Task
	if !tb.Eligible {
		return toolErr("not_task_eligible",
			fmt.Sprintf("skill %q is not a runnable task (no task.eligible: true)", name)), nil
	}
	kind := tb.Kind
	if kind == "" {
		kind = skill.TaskKindWorker
	}

	resp := describeResponse{
		Name:                sk.Manifest.Name,
		Description:         strings.TrimSpace(sk.Manifest.Description),
		GoalSummary:         strings.TrimSpace(tb.GoalSummary),
		Kind:                kind,
		RequiredInputs:      requiredKeys(tb.InputsSchema),
		AllowedToolsDefault: tb.AllowedToolsDefault,
	}
	if len(tb.InputsSchema) > 0 {
		raw, mErr := json.Marshal(tb.InputsSchema)
		if mErr != nil {
			return nil, fmt.Errorf("task:describe: marshal inputs_schema for %q: %w", name, mErr)
		}
		resp.InputsSchema = raw
	}
	body, err := json.Marshal(resp)
	if err != nil {
		return nil, fmt.Errorf("task:describe: marshal response: %w", err)
	}
	return body, nil
}

// requiredKeys extracts the `required` string list from a JSON-Schema
// inputs_schema (the generic decode yields []any). Always returns a
// non-nil slice so the JSON response carries `required_inputs: []` (not
// null) when nothing is mandatory. Non-string / empty entries skipped.
func requiredKeys(schema map[string]any) []string {
	out := []string{}
	req, ok := schema["required"].([]any)
	if !ok {
		return out
	}
	for _, r := range req {
		if s, ok := r.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}
