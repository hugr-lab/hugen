package skill

import (
	"errors"
	"testing"
)

// mustParseManifest parses or fails the test.
func mustParseManifest(t *testing.T, src string) Manifest {
	t.Helper()
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return m
}

// The exact run-2 dead-task shape: task block at top level + flat
// task_eligible. Parses clean (non-strict) but is not eligible.
const misplacedTaskSKILL = `---
name: tf-road-report
description: HTML report of roads by geozone
metadata:
  hugen:
    task_eligible: true
task:
  kind: worker
  goal_summary: build the report
  allowed_tools_default:
    - hugr-main:data-inline_graphql_result
tier_compatibility:
  - worker
---
body
`

const correctTaskSKILL = `---
name: tf-road-report
description: HTML report of roads by geozone
metadata:
  hugen:
    task:
      eligible: true
      kind: worker
      goal_summary: build the report
      allowed_tools_default:
        - hugr-main:data-inline_graphql_result
    tier_compatibility:
      - worker
---
body
`

const plainSKILL = `---
name: plain-helper
description: a non-task helper
metadata:
  hugen:
    tier_compatibility:
      - worker
---
body
`

func TestValidateTaskAuthoring_MisplacedIsCaught(t *testing.T) {
	m := mustParseManifest(t, misplacedTaskSKILL)
	// Sanity: the misplacement means the parsed block is NOT eligible.
	if m.Hugen.Task.Eligible {
		t.Fatal("precondition: misplaced task block must NOT parse as eligible")
	}
	err := ValidateTaskAuthoring(m)
	if err == nil {
		t.Fatal("ValidateTaskAuthoring accepted a misplaced task block")
	}
	if !errors.Is(err, ErrTaskBlockMisplaced) {
		t.Errorf("error does not wrap ErrTaskBlockMisplaced: %v", err)
	}
	// Both signals named.
	msg := err.Error()
	for _, want := range []string{"top-level `task:`", "task_eligible", "metadata.hugen.task"} {
		if !contains2(msg, want) {
			t.Errorf("error message missing %q: %s", want, msg)
		}
	}
}

func TestValidateTaskAuthoring_CorrectPasses(t *testing.T) {
	m := mustParseManifest(t, correctTaskSKILL)
	if !m.Hugen.Task.Eligible {
		t.Fatal("precondition: correctly-nested task must parse as eligible")
	}
	if err := ValidateTaskAuthoring(m); err != nil {
		t.Errorf("ValidateTaskAuthoring rejected a correct manifest: %v", err)
	}
}

func TestValidateTaskAuthoring_PlainSkillPasses(t *testing.T) {
	m := mustParseManifest(t, plainSKILL)
	if err := ValidateTaskAuthoring(m); err != nil {
		t.Errorf("ValidateTaskAuthoring rejected a plain non-task skill: %v", err)
	}
}

func contains2(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
