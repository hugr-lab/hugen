package task

import (
	"context"
	"encoding/json"
	"testing"
)

// callExecuteTask's argument + wiring guards short-circuit BEFORE any
// skill lookup or spawn, so they are unit-testable with a nil
// SkillManager. The resolve / eligibility / spawn path needs a real
// SkillManager + session and is harness/dogfood-validated.
func TestCallExecuteTask_GuardsReachableWithoutSkills(t *testing.T) {
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
			raw, err := e.callExecuteTask(context.Background(), json.RawMessage(tc.args))
			if err != nil {
				t.Fatalf("unexpected go error: %v", err)
			}
			if code := errCode(t, raw); code != tc.wantCode {
				t.Errorf("error code = %q, want %q (body %s)", code, tc.wantCode, raw)
			}
		})
	}
}

func TestExecuteTaskTool_Shape(t *testing.T) {
	tl := executeTaskTool()
	if tl.Name != "task:execute_task" {
		t.Errorf("name = %q, want task:execute_task", tl.Name)
	}
	if tl.Provider != providerName {
		t.Errorf("provider = %q, want %q", tl.Provider, providerName)
	}
	if tl.PermissionObject != PermExecuteTask {
		t.Errorf("perm = %q, want %q", tl.PermissionObject, PermExecuteTask)
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

// errCode unmarshals a structured toolErr body and returns its code,
// failing the test if the body is not a well-formed error envelope.
func errCode(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var resp toolErrorResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("unmarshal toolErr: %v (body %s)", err, raw)
	}
	if resp.OK {
		t.Fatalf("expected ok=false, got ok=true: %s", raw)
	}
	return resp.Error.Code
}
