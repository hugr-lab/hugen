package task

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hugr-lab/hugen/pkg/skill"
)

// extWithSearchCatalogue wires a task ext over an inline catalogue of one
// task-eligible skill and one plain skill, so search can be checked to
// return tasks only. The inline store has no embedder, so callSearch
// exercises the substring fallback path.
func extWithSearchCatalogue(t *testing.T) *Extension {
	t.Helper()
	taskManifest := describeTaskManifest("road_report", `
    task:
      eligible: true
      kind: worker
      goal_summary: Summarise road segments in a region.`)
	plainManifest := describeTaskManifest("road_helper", "")
	store := skill.NewSkillStore(skill.Options{Inline: map[string][]byte{
		"road_report": []byte(taskManifest),
		"road_helper": []byte(plainManifest),
	}})
	sm := skill.NewSkillManager(store, nil)
	for _, n := range []string{"road_report", "road_helper"} {
		if _, err := sm.Get(context.Background(), n); err != nil {
			t.Fatalf("inline skill %q did not parse/load: %v", n, err)
		}
	}
	return NewExtension(sm, "agt_test", nil)
}

func TestSearchTaskTool_Shape(t *testing.T) {
	tl := searchTaskTool()
	if tl.Name != "task:search" {
		t.Errorf("name = %q, want task:search", tl.Name)
	}
	if tl.Provider != providerName {
		t.Errorf("provider = %q, want %q", tl.Provider, providerName)
	}
	if tl.PermissionObject != PermSearchTask {
		t.Errorf("perm = %q, want %q", tl.PermissionObject, PermSearchTask)
	}
	var sch struct {
		Type     string   `json:"type"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(tl.ArgSchema, &sch); err != nil {
		t.Fatalf("ArgSchema is not valid JSON: %v", err)
	}
	if len(sch.Required) != 1 || sch.Required[0] != "query" {
		t.Errorf("schema required = %v, want [query]", sch.Required)
	}
}

func TestListEmitsSearch(t *testing.T) {
	e := extWithSearchCatalogue(t)
	tools, err := e.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var saw bool
	for _, tl := range tools {
		if tl.Name == "task:search" {
			saw = true
		}
	}
	if !saw {
		t.Error("List did not emit task:search")
	}
}

func TestCallSearch_GuardsWithoutSkills(t *testing.T) {
	e := NewExtension(nil, "agt_test", nil)
	cases := []struct {
		name     string
		args     string
		wantCode string
	}{
		{"malformed json", `{bad`, "invalid_args"},
		{"empty query", `{"query":"  "}`, "invalid_args"},
		{"no query key", `{}`, "invalid_args"},
		{"skills not wired", `{"query":"roads"}`, "no_skill_manager"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := e.callSearch(context.Background(), json.RawMessage(tc.args))
			if err != nil {
				t.Fatalf("unexpected go error: %v", err)
			}
			if code := errCode(t, raw); code != tc.wantCode {
				t.Errorf("error code = %q, want %q (body %s)", code, tc.wantCode, raw)
			}
		})
	}
}

// TestCallSearch_TasksOnly proves the substring fallback returns the
// task-eligible match and never the plain skill, even though both names
// contain "road".
func TestCallSearch_TasksOnly(t *testing.T) {
	e := extWithSearchCatalogue(t)
	raw, err := e.callSearch(context.Background(), json.RawMessage(`{"query":"road"}`))
	if err != nil {
		t.Fatalf("unexpected go error: %v", err)
	}
	var resp searchTaskResult
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode search response: %v (body %s)", err, raw)
	}
	if len(resp.Tasks) != 1 || resp.Tasks[0].Name != "road_report" {
		t.Fatalf("search = %+v, want only road_report (tasks only)", resp.Tasks)
	}
	if resp.Tasks[0].Description != "Summarise road segments in a region." {
		t.Errorf("description = %q, want the goal_summary", resp.Tasks[0].Description)
	}
}
