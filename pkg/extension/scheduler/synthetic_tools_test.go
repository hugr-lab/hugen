package scheduler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/skill"
)

func TestList_EmitsSyntheticToolsPerTaskEligibleSkill(t *testing.T) {
	ext := newExtWithSkills(t, map[string][]byte{
		"data_tables_rows_count": []byte(`---
name: data_tables_rows_count
description: Count rows in a Hugr data object.
license: MIT
metadata:
  hugen:
    tier_compatibility: [worker]
    task:
      eligible: true
      kind: worker
      goal_summary: Count rows via aggregation GraphQL.
      inputs_schema:
        type: object
        required: [data_object]
        properties:
          data_object: {type: string, description: "table/view name"}
---
body
`),
		"plain_skill": []byte(`---
name: plain_skill
description: Not a recipe.
license: MIT
---
body
`),
	})

	tools, err := ext.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	var synth *struct {
		Name        string
		Description string
		ArgSchema   json.RawMessage
	}
	for i := range tools {
		if tools[i].Name == "task:data_tables_rows_count" {
			synth = &struct {
				Name        string
				Description string
				ArgSchema   json.RawMessage
			}{
				Name:        tools[i].Name,
				Description: tools[i].Description,
				ArgSchema:   tools[i].ArgSchema,
			}
			break
		}
	}
	if synth == nil {
		var names []string
		for _, tt := range tools {
			names = append(names, tt.Name)
		}
		t.Fatalf("synthetic tool missing from List; got: %v", names)
	}
	if synth.Description != "Count rows via aggregation GraphQL." {
		t.Errorf("description: got %q, want goal_summary value", synth.Description)
	}
	var schema map[string]any
	if err := json.Unmarshal(synth.ArgSchema, &schema); err != nil {
		t.Fatalf("schema unmarshal: %v", err)
	}
	if schema["type"] != "object" {
		t.Errorf("schema.type = %v, want object", schema["type"])
	}
	props, _ := schema["properties"].(map[string]any)
	if _, ok := props["data_object"]; !ok {
		t.Errorf("schema missing data_object property: %+v", schema)
	}

	// Plain non-eligible skill must NOT contribute a synthetic tool.
	for _, tt := range tools {
		if tt.Name == "task:plain_skill" {
			t.Errorf("non-eligible skill leaked into catalogue as %s", tt.Name)
		}
	}
}

func TestList_StaticToolsAlwaysPresent(t *testing.T) {
	ext := newExtWithSkills(t, nil)
	tools, err := ext.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := map[string]bool{
		"task:create": false,
		"task:list":   false,
		"task:pause":  false,
		"task:resume": false,
		"task:cancel": false,
	}
	for _, tt := range tools {
		if _, ok := want[tt.Name]; ok {
			want[tt.Name] = true
		}
	}
	for n, present := range want {
		if !present {
			t.Errorf("static tool %s missing from List", n)
		}
	}
}

func TestList_NoSchemaSkillGetsPermissiveDefault(t *testing.T) {
	ext := newExtWithSkills(t, map[string][]byte{
		"loose_recipe": []byte(`---
name: loose_recipe
description: No inputs schema declared.
license: MIT
metadata:
  hugen:
    tier_compatibility: [worker]
    task:
      eligible: true
      kind: worker
---
body
`),
	})
	tools, err := ext.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var argSchema string
	for _, tt := range tools {
		if tt.Name == "task:loose_recipe" {
			argSchema = string(tt.ArgSchema)
			break
		}
	}
	if argSchema == "" {
		t.Fatal("loose_recipe synthetic tool missing")
	}
	if !strings.Contains(argSchema, `"type":"object"`) {
		t.Errorf("default schema malformed: %s", argSchema)
	}
	if !strings.Contains(argSchema, `"additionalProperties":true`) {
		t.Errorf("default schema should permit additionalProperties: %s", argSchema)
	}
}

func TestList_SyntheticToolNameIsTaskPrefixed(t *testing.T) {
	ext := newExtWithSkills(t, map[string][]byte{
		"abc": []byte(`---
name: abc
description: x
license: MIT
metadata:
  hugen:
    tier_compatibility: [worker]
    task: {eligible: true, kind: worker}
---
`),
	})
	tools, _ := ext.List(context.Background())
	var found bool
	for _, tt := range tools {
		if tt.Name == "task:abc" {
			found = true
			if tt.Provider != "task" {
				t.Errorf("Provider = %q, want task", tt.Provider)
			}
			if tt.PermissionObject != PermRunRecipe {
				t.Errorf("PermissionObject = %q, want %q", tt.PermissionObject, PermRunRecipe)
			}
			break
		}
	}
	if !found {
		t.Error("task:abc not emitted")
	}
}

// Compile-time assertion: TestList_… helpers compile against the
// existing skill.SkillManager + skill.Skill shapes used by the
// scheduler ext.
var _ = skill.Skill{}
