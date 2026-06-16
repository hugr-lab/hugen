package task

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/session"
)

// RunRecipe's guard clauses short-circuit BEFORE any session spawn, so
// they are unit-testable without the fixture harness. The happy spawn →
// kick → wait path needs a real *session.Session (booting the session
// machinery) and is exercised by live dogfood + the scenario harness —
// same precedent as the scheduler spawn-fire tests (see
// pkg/extension/scheduler/fake_test.go).

func TestRunRecipe_NilAnchorErrors(t *testing.T) {
	e := NewExtension(nil, "agt_test", nil)
	_, err := e.RunRecipe(context.Background(), RunParams{
		// Anchor nil — the first guard fires before SpawnName is read.
		SpawnName: "task-x-1",
	})
	if err == nil {
		t.Fatal("expected error on nil anchor")
	}
	if !strings.Contains(err.Error(), "nil anchor") {
		t.Errorf("error = %q, want mention of nil anchor", err)
	}
}

func TestRunRecipe_EmptySpawnNameErrors(t *testing.T) {
	e := NewExtension(nil, "agt_test", nil)
	// Non-nil anchor sentinel: the empty-SpawnName guard fires before
	// Anchor.Spawn is ever called, so the zero-value session is never
	// dereferenced.
	_, err := e.RunRecipe(context.Background(), RunParams{
		Anchor:    &session.Session{},
		SpawnName: "",
	})
	if err == nil {
		t.Fatal("expected error on empty spawn name")
	}
	if !strings.Contains(err.Error(), "spawn name") {
		t.Errorf("error = %q, want mention of spawn name", err)
	}
}

func TestDecodeInputs(t *testing.T) {
	cases := []struct {
		name    string
		args    string
		wantNil bool
		wantErr bool
	}{
		{"empty", "", true, false},
		{"null literal", "null", true, false},
		{"object", `{"region":"EU"}`, false, false},
		{"array", `[1,2,3]`, false, false},
		{"malformed", `{bad`, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeInputs(json.RawMessage(tc.args))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("decodeInputs(%q) expected error", tc.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("decodeInputs(%q) unexpected error: %v", tc.args, err)
			}
			if tc.wantNil && got != nil {
				t.Errorf("decodeInputs(%q) = %v, want nil", tc.args, got)
			}
			if !tc.wantNil && got == nil {
				t.Errorf("decodeInputs(%q) = nil, want non-nil", tc.args)
			}
		})
	}
}

func TestBuildFirstMessage(t *testing.T) {
	const task = "Run the report recipe once with the supplied inputs."

	cases := []struct {
		name      string
		inputs    any
		wantBlock bool // true → expects [Inputs] block prepended
	}{
		{"nil inputs", nil, false},
		{"empty map", map[string]any{}, false},
		{"empty slice", []any{}, false},
		{"populated map", map[string]any{"region": "EU"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildFirstMessage(task, tc.inputs)
			hasBlock := strings.Contains(got, "[Inputs]") && strings.Contains(got, "[Task]")
			if hasBlock != tc.wantBlock {
				t.Errorf("buildFirstMessage block=%v want %v (got %q)", hasBlock, tc.wantBlock, got)
			}
			if !strings.Contains(got, task) {
				t.Errorf("buildFirstMessage dropped the task body: %q", got)
			}
		})
	}
}
