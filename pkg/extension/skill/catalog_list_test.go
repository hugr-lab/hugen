package skill

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/internal/fixture"
	skillpkg "github.com/hugr-lab/hugen/pkg/skill"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// newCatalogListFixture wires a SkillManager over two inline skills:
//   - `change-report`: a task-eligible recipe (task block + inputs
//     schema + mission keywords);
//   - `plain-helper`: a non-task skill with a mission summary +
//     keyword but no task block.
//
// No ToolManager is needed — catalog_list reads the SkillManager,
// not the live tool snapshot.
func newCatalogListFixture(t *testing.T) (*Extension, *fixture.TestSessionState) {
	t.Helper()
	store := skillpkg.NewSkillStore(skillpkg.Options{Inline: map[string][]byte{
		"change-report": []byte(`---
name: change-report
description: Summarise what changed in a dataset over a window.
metadata:
  hugen:
    task:
      eligible: true
      kind: worker
      goal_summary: Produce a change report for the given dataset and window.
      inputs_schema:
        type: object
        required: [dataset]
        properties:
          dataset: {type: string}
    mission:
      keywords: [change, report, diff]
compatibility:
  model: any
  runtime: hugen-phase-6
---
body
`),
		"plain-helper": []byte(`---
name: plain-helper
description: A plain non-task helper skill.
metadata:
  hugen:
    mission:
      summary: helps with things
      keywords: [helper, misc]
compatibility:
  model: any
  runtime: hugen-phase-6
---
body
`),
	}})
	mgr := skillpkg.NewSkillManager(store, nil)
	ext := NewExtension(mgr, nil, "agent-catlist")
	state := fixture.NewTestSessionState("ses-catlist").WithDepth(2)
	if err := ext.InitState(context.Background(), state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	return ext, state
}

func callCatalogList(t *testing.T, ext *Extension, state *fixture.TestSessionState, args string) catalogListResult {
	t.Helper()
	out, err := ext.Call(extension.WithSessionState(context.Background(), state),
		"skill:catalog_list", json.RawMessage(args))
	if err != nil {
		t.Fatalf("Call(%s): %v", args, err)
	}
	var got catalogListResult
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	return got
}

func TestCatalogList_RegisteredOnSkillProvider(t *testing.T) {
	ext, _ := newCatalogListFixture(t)
	tools, err := ext.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, tt := range tools {
		if tt.Name == "skill:catalog_list" {
			if tt.PermissionObject != permObjectCatalogList {
				t.Errorf("perm = %q, want %q", tt.PermissionObject, permObjectCatalogList)
			}
			return
		}
	}
	t.Errorf("skill:catalog_list not registered on provider")
}

func TestCatalogList_ExcludesTasks(t *testing.T) {
	ext, state := newCatalogListFixture(t)
	got := callCatalogList(t, ext, state, `{}`)
	// change-report is a built task → EXCLUDED; only plain-helper lists.
	if len(got.Skills) != 1 || got.Skills[0].Name != "plain-helper" {
		t.Fatalf("catalog_list = %+v, want only plain-helper (tasks excluded)", got.Skills)
	}
	for _, s := range got.Skills {
		if s.Name == "change-report" {
			t.Errorf("task change-report leaked into skill catalogue")
		}
	}
}

func TestCatalogList_KeywordFilter(t *testing.T) {
	ext, state := newCatalogListFixture(t)
	// A keyword that ONLY matches the task (change-report's "diff"
	// keyword) returns nothing — tasks are excluded from skill search.
	got := callCatalogList(t, ext, state, `{"keyword":"DIFF"}`)
	if len(got.Skills) != 0 {
		t.Fatalf("keyword=diff = %+v, want empty (only a task matched)", got.Skills)
	}
	// Keyword matching a non-task skill's description (case-insensitive).
	got = callCatalogList(t, ext, state, `{"keyword":"non-task"}`)
	if len(got.Skills) != 1 || got.Skills[0].Name != "plain-helper" {
		t.Fatalf("keyword=non-task = %+v, want only plain-helper", got.Skills)
	}
	// No match → empty list, not nil decode error.
	got = callCatalogList(t, ext, state, `{"keyword":"nonsense-xyz"}`)
	if len(got.Skills) != 0 {
		t.Fatalf("keyword=nonsense = %+v, want empty", got.Skills)
	}
}

// contains reports whether want is an element of xs (shared by the
// catalogue + capability tests).
func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func TestCatalogList_BadRequest(t *testing.T) {
	ext, state := newCatalogListFixture(t)
	_, err := ext.Call(extension.WithSessionState(context.Background(), state),
		"skill:catalog_list", json.RawMessage(`{not-json`))
	if err == nil {
		t.Fatalf("expected ErrArgValidation, got nil")
	}
	if !errors.Is(err, tool.ErrArgValidation) {
		t.Errorf("expected ErrArgValidation, got %v", err)
	}
}
