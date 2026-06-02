package session

import (
	"testing"

	"github.com/hugr-lab/hugen/pkg/model"
)

// Phase 5.2 budget-termination — session-side context-budget threshold
// sampling. Single threshold (no soft/hard); enforcement itself rides
// the tool-dispatch path (dispatchToolCall short-circuits once the
// turnState latches budgetExceeded).

func TestApplyContextBudget_RootSkipped(t *testing.T) {
	// parent == nil → root session → budget check disabled (left 0).
	router := newRouterWithModel(t, &scriptedModel{})
	router.SetContextBudgets(map[model.ModelSpec]int{{Provider: "fake", Name: "test"}: 100_000}, 0, nil, nil)
	s := &Session{models: router, turnState: &turnState{}}
	s.applyContextBudget()
	if s.turnState.budgetThreshold != 0 {
		t.Fatalf("root session must not arm the budget; got threshold=%d", s.turnState.budgetThreshold)
	}
}

func TestApplyContextBudget_SubagentSet(t *testing.T) {
	router := newRouterWithModel(t, &scriptedModel{})
	router.SetContextBudgets(map[model.ModelSpec]int{{Provider: "fake", Name: "test"}: 100_000}, 0, nil, nil)
	s := &Session{models: router, parent: &Session{}, turnState: &turnState{}}
	s.SetDefaultIntent(model.IntentDefault)
	s.applyContextBudget()
	want := int(model.DefaultContextBudgetRatio * 100_000) // 0.85 × 100k
	if s.turnState.budgetThreshold != want {
		t.Fatalf("subagent budget threshold = %d, want %d", s.turnState.budgetThreshold, want)
	}
}
