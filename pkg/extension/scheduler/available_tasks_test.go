package scheduler

import (
	"context"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/skill"
)

// newExtWithSkills builds an Extension wired to an inline SkillStore.
// `inline` maps skill-name → raw frontmatter+body bytes; the store
// parses them via the production manifest parser so the test exercises
// the same path production runs.
func newExtWithSkills(t *testing.T, inline map[string][]byte) *Extension {
	t.Helper()
	store := skill.NewSkillStore(skill.Options{Inline: inline})
	m := skill.NewSkillManager(store, nil)
	return NewExtension(nil, m, "agt-test", nil)
}

func TestAvailableTasks_EmptyWhenNoEligibleSkills(t *testing.T) {
	ext := newExtWithSkills(t, map[string][]byte{
		"loose": []byte(`---
name: loose
description: not task-eligible
license: MIT
---
body
`),
	})
	state := newFakeState("ses-root")
	out := ext.AdvertiseSystemPrompt(context.Background(), state)
	if out != "" {
		t.Errorf("non-eligible catalogue must render empty; got:\n%s", out)
	}
}

func TestAvailableTasks_RootSeesEligibleSkills(t *testing.T) {
	ext := newExtWithSkills(t, map[string][]byte{
		"daily_report": []byte(`---
name: daily_report
description: Generates the daily revenue dashboard summary.
license: MIT
metadata:
  hugen:
    task:
      eligible: true
      kind: worker
      goal_summary: Summarise yesterday's revenue dashboard.
---
body
`),
		"loose": []byte(`---
name: loose
description: not task-eligible
license: MIT
---
body
`),
	})
	state := newFakeState("ses-root")
	out := ext.AdvertiseSystemPrompt(context.Background(), state)
	if !strings.Contains(out, "## Available tasks") {
		t.Errorf("heading missing:\n%s", out)
	}
	if !strings.Contains(out, "`daily_report`") {
		t.Errorf("eligible skill missing:\n%s", out)
	}
	if !strings.Contains(out, "kind=worker") {
		t.Errorf("kind tag missing:\n%s", out)
	}
	if !strings.Contains(out, "Summarise yesterday's revenue dashboard.") {
		t.Errorf("goal_summary missing:\n%s", out)
	}
	if strings.Contains(out, "`loose`") {
		t.Errorf("non-eligible skill leaked into catalogue:\n%s", out)
	}
	// Two paths advertised so the LLM knows it's not just task:create.
	if !strings.Contains(out, "spawn_mission(skill=\"_run_task\"") {
		t.Errorf("ad-hoc path missing:\n%s", out)
	}
	if !strings.Contains(out, "task:create") {
		t.Errorf("scheduled path missing:\n%s", out)
	}
}

func TestAvailableTasks_FallsBackToManifestDescription(t *testing.T) {
	ext := newExtWithSkills(t, map[string][]byte{
		"sparse": []byte(`---
name: sparse
description: Fallback description shown when goal_summary is empty.
license: MIT
metadata:
  hugen:
    task:
      eligible: true
---
body
`),
	})
	state := newFakeState("ses-root")
	out := ext.AdvertiseSystemPrompt(context.Background(), state)
	if !strings.Contains(out, "Fallback description shown when goal_summary is empty.") {
		t.Errorf("description fallback missing:\n%s", out)
	}
}

func TestAvailableTasks_SortedByName(t *testing.T) {
	ext := newExtWithSkills(t, map[string][]byte{
		"zeta_task": []byte(`---
name: zeta_task
description: zeta
license: MIT
metadata: {hugen: {task: {eligible: true}}}
---
body
`),
		"alpha_task": []byte(`---
name: alpha_task
description: alpha
license: MIT
metadata: {hugen: {task: {eligible: true}}}
---
body
`),
	})
	state := newFakeState("ses-root")
	out := ext.AdvertiseSystemPrompt(context.Background(), state)
	idxAlpha := strings.Index(out, "alpha_task")
	idxZeta := strings.Index(out, "zeta_task")
	if idxAlpha == -1 || idxZeta == -1 || idxAlpha > idxZeta {
		t.Errorf("catalogue must be alpha-sorted (alpha@%d, zeta@%d):\n%s",
			idxAlpha, idxZeta, out)
	}
}

func TestAvailableTasks_RendersInputsSchema(t *testing.T) {
	ext := newExtWithSkills(t, map[string][]byte{
		"data_row_count": []byte(`---
name: data_row_count
description: Count rows in a Hugr data object.
license: MIT
metadata:
  hugen:
    task:
      eligible: true
      kind: worker
      goal_summary: Count rows in a Hugr data object.
      inputs_schema:
        type: object
        required: [data_object]
        properties:
          data_object:
            type: string
            description: The data object (table/view) to count.
          module:
            type: string
            description: Optional module path.
---
body
`),
	})
	state := newFakeState("ses-root")
	out := ext.AdvertiseSystemPrompt(context.Background(), state)
	if !strings.Contains(out, "inputs (required):") {
		t.Errorf("required block missing:\n%s", out)
	}
	if !strings.Contains(out, "data_object (string) — The data object (table/view) to count.") {
		t.Errorf("required entry missing:\n%s", out)
	}
	if !strings.Contains(out, "inputs (optional):") {
		t.Errorf("optional block missing:\n%s", out)
	}
	if !strings.Contains(out, "module (string) — Optional module path.") {
		t.Errorf("optional entry missing:\n%s", out)
	}
}

func TestAvailableTasks_NilSkillManagerReturnsEmpty(t *testing.T) {
	ext := NewExtension(nil, nil, "agt-test", nil)
	state := newFakeState("ses-root")
	if got := ext.AdvertiseSystemPrompt(context.Background(), state); got != "" {
		t.Errorf("nil SkillManager must render empty; got:\n%s", got)
	}
}
