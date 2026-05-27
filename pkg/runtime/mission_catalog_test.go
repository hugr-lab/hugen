package runtime

import (
	"testing"

	missionext "github.com/hugr-lab/hugen/pkg/extension/mission"
	skillpkg "github.com/hugr-lab/hugen/pkg/skill"
)

// TestProjectMissionManifest_CapabilitiesFlowsThrough verifies that
// Phase-F manifest knobs land on the typed mission projection the
// mission ext consumes. Both mission-tier toggles and per-role
// capabilities round-trip; sub_agents with no declared caps are
// omitted from the Roles map (mission ext falls back to role-class
// defaults).
func TestProjectMissionManifest_CapabilitiesFlowsThrough(t *testing.T) {
	tru := true
	fls := false
	m := skillpkg.Manifest{
		Name: "_test_caps",
		Hugen: skillpkg.HugenMetadata{
			Mission: skillpkg.MissionBlock{
				Summary: "Phase F caps round-trip fixture.",
				Plan: skillpkg.MissionPlanBlock{
					Role: "planner-role",
				},
				Capabilities: skillpkg.MissionCapabilities{
					Notepad:     &tru,
					Whiteboard:  &fls,
					PlanContext: nil,
				},
			},
			SubAgents: []skillpkg.SubAgentRole{
				{
					Name: "planner-role",
					Capabilities: skillpkg.SubAgentCapabilities{
						PlanContext: "read",
					},
				},
				{
					Name: "echo-do",
					Capabilities: skillpkg.SubAgentCapabilities{
						PlanContext: "off",
					},
				},
				{
					Name: "no-caps-role",
				},
			},
		},
	}

	out := projectMissionManifest(m)
	if out == nil {
		t.Fatalf("projectMissionManifest returned nil — Plan.Role should make it eligible")
	}

	// Mission-tier capability toggles round-trip.
	if got := out.Capabilities.Notepad; got == nil || !*got {
		t.Errorf("Capabilities.Notepad = %v, want *true", got)
	}
	if got := out.Capabilities.Whiteboard; got == nil || *got {
		t.Errorf("Capabilities.Whiteboard = %v, want *false", got)
	}
	if got := out.Capabilities.PlanContext; got != nil {
		t.Errorf("Capabilities.PlanContext = %v, want nil", got)
	}

	// Roles map includes only declared-caps roles.
	if len(out.Roles) != 2 {
		t.Fatalf("Roles map size = %d, want 2 (planner-role + echo-do)", len(out.Roles))
	}
	if got := out.Roles["planner-role"].PlanContextAccess; got != "read" {
		t.Errorf("Roles[planner-role].PlanContextAccess = %q, want read", got)
	}
	if got := out.Roles["echo-do"].PlanContextAccess; got != "off" {
		t.Errorf("Roles[echo-do].PlanContextAccess = %q, want off", got)
	}
	if _, ok := out.Roles["no-caps-role"]; ok {
		t.Errorf("Roles must not include roles with no declared capabilities")
	}

	// Resolver picks up the projected per-role data + role-class
	// default for no-caps-role.
	if got := missionext.ResolvePlanContextAccess(*out, "planner-role"); got != missionext.PlanContextRead {
		t.Errorf("ResolvePlanContextAccess(planner-role) = %q, want read", got)
	}
	if got := missionext.ResolvePlanContextAccess(*out, "echo-do"); got != missionext.PlanContextOff {
		t.Errorf("ResolvePlanContextAccess(echo-do) = %q, want off (explicit override)", got)
	}
	if got := missionext.ResolvePlanContextAccess(*out, "no-caps-role"); got != missionext.PlanContextOff {
		t.Errorf("ResolvePlanContextAccess(no-caps-role) = %q, want off (Do-role default)", got)
	}
}

// TestProjectMissionManifest_InputsSchemaFlowsThrough verifies that
// `mission.inputs_schema` declared in the skill manifest reaches
// MissionManifest.InputsSchema after projection — the surface root's
// Available missions advertiser will render. Phase 6.1d.
func TestProjectMissionManifest_InputsSchemaFlowsThrough(t *testing.T) {
	schema := map[string]any{
		"type":     "object",
		"required": []any{"task_skill"},
		"properties": map[string]any{
			"task_skill": map[string]any{
				"type":        "string",
				"description": "Recipe name from Available tasks",
			},
		},
	}
	m := skillpkg.Manifest{
		Name: "_test_inputs_schema",
		Hugen: skillpkg.HugenMetadata{
			Mission: skillpkg.MissionBlock{
				Summary:      "Inputs-schema round-trip fixture.",
				InputsSchema: schema,
				Plan:         skillpkg.MissionPlanBlock{Role: "planner"},
			},
		},
	}
	out := projectMissionManifest(m)
	if out == nil {
		t.Fatal("projectMissionManifest returned nil")
	}
	if out.InputsSchema == nil {
		t.Fatal("InputsSchema dropped during projection")
	}
	if got, _ := out.InputsSchema["type"].(string); got != "object" {
		t.Errorf("InputsSchema.type = %v, want object", out.InputsSchema["type"])
	}
	if got, _ := out.InputsSchema["required"].([]any); len(got) != 1 || got[0] != "task_skill" {
		t.Errorf("InputsSchema.required = %v, want [task_skill]", out.InputsSchema["required"])
	}
}

// TestProjectMissionManifest_InputsSchemaAbsent verifies the
// projection leaves InputsSchema nil when the manifest declares no
// schema (the common case for legacy mission skills).
func TestProjectMissionManifest_InputsSchemaAbsent(t *testing.T) {
	m := skillpkg.Manifest{
		Name: "_test_no_schema",
		Hugen: skillpkg.HugenMetadata{
			Mission: skillpkg.MissionBlock{
				Summary: "No schema declared.",
				Plan:    skillpkg.MissionPlanBlock{Role: "planner"},
			},
		},
	}
	out := projectMissionManifest(m)
	if out == nil {
		t.Fatal("projectMissionManifest returned nil")
	}
	if out.InputsSchema != nil {
		t.Errorf("InputsSchema = %v, want nil", out.InputsSchema)
	}
}
