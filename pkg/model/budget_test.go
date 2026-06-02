package model

import "testing"

// Phase 5.2 budget-termination — router budget accessors.

func TestMaxPromptTokens(t *testing.T) {
	specBig := ModelSpec{Provider: "p", Name: "big"}
	specSmall := ModelSpec{Provider: "p", Name: "small"}
	r := newRouter(t, map[Intent]ModelSpec{
		IntentDefault:     specBig,
		IntentCheap:       specSmall,
		IntentToolCalling: specBig,
	})

	// No budgets configured yet → 0 (unlimited; budget guard off).
	if got := r.MaxPromptTokens(IntentDefault); got != 0 {
		t.Fatalf("pre-wire default = %d, want 0 (unconfigured → unlimited)", got)
	}

	r.SetContextBudgets(
		map[ModelSpec]int{specBig: 200_000},
		50_000, // DefaultBudget for specs with no window
		map[Intent]float64{IntentCheap: 0.7},
		nil,
	)

	cases := []struct {
		intent Intent
		want   int
	}{
		{IntentDefault, 200_000},     // explicit window
		{IntentToolCalling, 200_000}, // shares specBig
		{IntentCheap, 50_000},        // no window → DefaultBudget
		{Intent("unknown"), 200_000}, // falls back to IntentDefault spec
		{Intent(""), 200_000},        // empty → IntentDefault
	}
	for _, tc := range cases {
		if got := r.MaxPromptTokens(tc.intent); got != tc.want {
			t.Errorf("MaxPromptTokens(%q) = %d, want %d", tc.intent, got, tc.want)
		}
	}
}

func TestMaxPromptTokens_PerIntentOverride(t *testing.T) {
	spec := ModelSpec{Provider: "p", Name: "shared"}
	// A dedicated "worker" intent shares the model with default but
	// carries its own (tighter) budget.
	r := newRouter(t, map[Intent]ModelSpec{IntentDefault: spec, Intent("worker"): spec})
	r.SetContextBudgets(
		map[ModelSpec]int{spec: 200_000}, // shared model window
		0, nil,
		map[Intent]int{Intent("worker"): 40_000}, // per-intent override
	)
	if got := r.MaxPromptTokens(Intent("worker")); got != 40_000 {
		t.Errorf("worker MaxPromptTokens = %d, want 40000 (per-intent override)", got)
	}
	if got := r.MaxPromptTokens(IntentDefault); got != 200_000 {
		t.Errorf("default MaxPromptTokens = %d, want 200000 (model window, not the worker override)", got)
	}
}

func TestMaxPromptTokens_UnconfiguredIsUnlimited(t *testing.T) {
	spec := ModelSpec{Provider: "p", Name: "m"}
	r := newRouter(t, map[Intent]ModelSpec{IntentDefault: spec})

	// No windows, no DefaultBudget → 0 (unlimited; budget guard off).
	r.SetContextBudgets(nil, 0, nil, nil)
	if got := r.MaxPromptTokens(IntentDefault); got != 0 {
		t.Fatalf("got %d, want 0 (unconfigured → unlimited)", got)
	}
	// A global DefaultBudget set → DefaultBudget.
	r.SetContextBudgets(map[ModelSpec]int{}, 64_000, nil, nil)
	if got := r.MaxPromptTokens(IntentDefault); got != 64_000 {
		t.Fatalf("got %d, want DefaultBudget 64000", got)
	}
}

func TestContextBudgetRatio(t *testing.T) {
	spec := ModelSpec{Provider: "p", Name: "m"}
	r := newRouter(t, map[Intent]ModelSpec{IntentDefault: spec, IntentCheap: spec})
	r.SetContextBudgets(nil, 0, map[Intent]float64{IntentCheap: 0.5}, nil)

	// Default intent: no override → package default.
	if got := r.ContextBudgetRatio(IntentDefault); got != DefaultContextBudgetRatio {
		t.Fatalf("default ratio = %v, want %v", got, DefaultContextBudgetRatio)
	}
	// Per-route override wins.
	if got := r.ContextBudgetRatio(IntentCheap); got != 0.5 {
		t.Fatalf("cheap ratio = %v, want 0.5", got)
	}
	// Unknown intent → default.
	if got := r.ContextBudgetRatio(Intent("unknown")); got != DefaultContextBudgetRatio {
		t.Fatalf("unknown ratio = %v, want %v", got, DefaultContextBudgetRatio)
	}
}
