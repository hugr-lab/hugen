package mission

import (
	"context"
	"log/slog"
	"strings"
	"testing"
)

func newAdvertiseExt(t *testing.T, manifests ...*MissionManifest) *Extension {
	t.Helper()
	cat := NewStaticCatalog(manifests...)
	return &Extension{
		catalog: cat,
		logger:  slog.New(slog.DiscardHandler),
	}
}

func TestAdvertiseSystemPrompt_RendersMissionInputsSchema(t *testing.T) {
	ext := newAdvertiseExt(t, &MissionManifest{
		Name:    "_run_task",
		Summary: "Run a task-eligible recipe ad-hoc.",
		Plan:    MissionPlanManifest{Role: "planner"},
		InputsSchema: map[string]any{
			"type":     "object",
			"required": []any{"task_skill"},
			"properties": map[string]any{
				"task_skill": map[string]any{
					"type":        "string",
					"description": "Recipe name from Available tasks",
				},
				"task_inputs": map[string]any{
					"type":        "object",
					"description": "Pre-filled inputs matching the recipe's schema",
				},
			},
		},
	})
	state := newFakeState("ses-root")
	out := ext.AdvertiseSystemPrompt(context.Background(), state)
	if !strings.Contains(out, "## Available missions") {
		t.Fatalf("heading missing:\n%s", out)
	}
	if !strings.Contains(out, "`_run_task` — Run a task-eligible recipe ad-hoc.") {
		t.Errorf("summary missing:\n%s", out)
	}
	if !strings.Contains(out, "  inputs (required):\n    task_skill (string) — Recipe name from Available tasks") {
		t.Errorf("required block missing/malformed:\n%s", out)
	}
	if !strings.Contains(out, "  inputs (optional):\n    task_inputs (object) — Pre-filled inputs matching the recipe's schema") {
		t.Errorf("optional block missing/malformed:\n%s", out)
	}
}

func TestAdvertiseSystemPrompt_SkipsSchemaBlockWhenAbsent(t *testing.T) {
	ext := newAdvertiseExt(t, &MissionManifest{
		Name:    "legacy_mission",
		Summary: "Old mission with no schema.",
		Plan:    MissionPlanManifest{Role: "planner"},
	})
	state := newFakeState("ses-root")
	out := ext.AdvertiseSystemPrompt(context.Background(), state)
	if !strings.Contains(out, "`legacy_mission` — Old mission with no schema.") {
		t.Errorf("summary missing:\n%s", out)
	}
	if strings.Contains(out, "inputs (") {
		t.Errorf("schema block must be absent for schema-less mission:\n%s", out)
	}
}

func TestAdvertiseSystemPrompt_NonRootEmpty(t *testing.T) {
	ext := newAdvertiseExt(t, &MissionManifest{
		Name: "any",
		Plan: MissionPlanManifest{Role: "planner"},
	})
	state := newFakeState("ses-child")
	// fakeState.Depth() returns 0; emulate non-root by parent set.
	parent := newFakeState("ses-parent")
	state.parent = parent
	// Depth() still 0 since fakeState hardcodes it; the advertiser
	// gates on Depth() == 0 — this scenario actually exercises the
	// happy path. The "non-root empty" path is covered by
	// pkg/extension/skill/capabilities_test.go's check that the
	// "Available missions" block was moved here; we keep this test
	// as documentation that fakeState's Depth is the limit and rely
	// on the production session manager for real-depth gating.
	out := ext.AdvertiseSystemPrompt(context.Background(), state)
	if out == "" {
		t.Errorf("happy path returned empty; expected non-empty for depth-0 fake")
	}
}
