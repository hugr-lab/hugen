package runtime

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/assets"
	missionext "github.com/hugr-lab/hugen/pkg/extension/mission"
	skillpkg "github.com/hugr-lab/hugen/pkg/skill"
)

// TestProjectMissionManifest_AnalystStagesEndToEnd parses the
// embedded analyst SKILL.md and projects it, asserting the real
// research-stage hooks reach the typed projection. Guards against
// drift between the shipped manifest and the runtime that fires the
// hooks. Phase 6.x — research→files.
func TestProjectMissionManifest_AnalystStagesEndToEnd(t *testing.T) {
	raw, err := fs.ReadFile(assets.SkillsFS, "skills/analyst/SKILL.md")
	if err != nil {
		t.Fatalf("read embedded analyst SKILL.md: %v", err)
	}
	m, err := skillpkg.Parse(raw)
	if err != nil {
		t.Fatalf("parse analyst manifest: %v", err)
	}
	out := projectMissionManifest(m, "/skills/hub/analyst")
	if out == nil {
		t.Fatal("analyst is not projected as a mission")
	}

	before := out.Stages.Research.Before
	if before == nil || before.Tool != "bash-mcp:bash.shell" {
		t.Fatalf("research before hook = %+v, want bash-mcp:bash.shell", before)
	}
	cmd, _ := before.Args["cmd"].(string)
	if !strings.Contains(cmd, "{{.MissionSkill}}/templates/research") ||
		!strings.Contains(cmd, "{{.MissionDir}}/research") {
		t.Errorf("scaffold cmd missing templated paths: %q", cmd)
	}

	check := out.Stages.Research.Check
	if check == nil || check.Tool != "bash-mcp:bash.shell" {
		t.Fatalf("research check hook = %+v, want bash-mcp:bash.shell", check)
	}
	ccmd, _ := check.Args["cmd"].(string)
	if !strings.Contains(ccmd, "check_research.py") || !strings.Contains(ccmd, "{{.MissionDir}}") {
		t.Errorf("check cmd missing script/path: %q", ccmd)
	}
}

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

	out := projectMissionManifest(m, "")
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
	out := projectMissionManifest(m, "")
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
	out := projectMissionManifest(m, "")
	if out == nil {
		t.Fatal("projectMissionManifest returned nil")
	}
	if out.InputsSchema != nil {
		t.Errorf("InputsSchema = %v, want nil", out.InputsSchema)
	}
}

// TestProjectMissionManifest_StagesAndSkillDir verifies the research
// stage's before/check hooks + the skill's on-disk dir reach the
// typed projection so the runtime can fire scaffold + gate hooks.
// Phase 6.x — research→files.
func TestProjectMissionManifest_StagesAndSkillDir(t *testing.T) {
	m := skillpkg.Manifest{
		Name: "_test_stages",
		Hugen: skillpkg.HugenMetadata{
			Mission: skillpkg.MissionBlock{
				Summary: "Stages round-trip fixture.",
				Plan:    skillpkg.MissionPlanBlock{Role: "planner"},
				Stages: skillpkg.MissionStages{
					Research: skillpkg.MissionStageHooks{
						Before: &skillpkg.MissionStageHook{
							Tool: "bash:run",
							Args: map[string]any{"command": "scaffold {{.MissionDir}}"},
						},
						Check: &skillpkg.MissionStageHook{
							Tool: "python:run_script",
							Args: map[string]any{"path": "check.py"},
						},
					},
				},
			},
		},
	}
	out := projectMissionManifest(m, "/skills/hub/analyst")
	if out == nil {
		t.Fatal("projectMissionManifest returned nil")
	}
	if out.SkillDir != "/skills/hub/analyst" {
		t.Errorf("SkillDir = %q, want /skills/hub/analyst", out.SkillDir)
	}
	if out.Stages.Research.Before == nil || out.Stages.Research.Before.Tool != "bash:run" {
		t.Fatalf("research before hook = %+v, want bash:run", out.Stages.Research.Before)
	}
	if got, _ := out.Stages.Research.Before.Args["command"].(string); got != "scaffold {{.MissionDir}}" {
		t.Errorf("before hook command = %q, want the raw template (rendered at fire time)", got)
	}
	if out.Stages.Research.Check == nil || out.Stages.Research.Check.Tool != "python:run_script" {
		t.Fatalf("research check hook = %+v, want python:run_script", out.Stages.Research.Check)
	}
}

// TestProjectMissionManifest_EmptyToolHookDropped verifies a hook
// with a blank tool projects to nil (no-op) rather than a hook that
// would fail at dispatch. Phase 6.x.
func TestProjectMissionManifest_EmptyToolHookDropped(t *testing.T) {
	m := skillpkg.Manifest{
		Name: "_test_empty_hook",
		Hugen: skillpkg.HugenMetadata{
			Mission: skillpkg.MissionBlock{
				Plan: skillpkg.MissionPlanBlock{Role: "planner"},
				Stages: skillpkg.MissionStages{
					Research: skillpkg.MissionStageHooks{
						Before: &skillpkg.MissionStageHook{Tool: "  "},
					},
				},
			},
		},
	}
	out := projectMissionManifest(m, "")
	if out == nil {
		t.Fatal("projectMissionManifest returned nil")
	}
	if out.Stages.Research.Before != nil {
		t.Errorf("blank-tool hook = %+v, want nil", out.Stages.Research.Before)
	}
}
