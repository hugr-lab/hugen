package session

import (
	"context"
	"testing"

	"github.com/hugr-lab/hugen/pkg/extension"
)

// fakeTurnBudgetExt is a minimal extension implementing
// TurnBudgetLookup. Returns whatever .budget is configured to;
// lets tests pin the resolver's layer-by-layer behaviour.
type fakeTurnBudgetExt struct {
	budget   extension.TurnBudget
	lastCall struct{ spawnSkill, spawnRole string }
}

func (f *fakeTurnBudgetExt) Name() string { return "fake-budget" }

func (f *fakeTurnBudgetExt) ResolveTurnBudget(_ context.Context, _ extension.SessionState, spawnSkill, spawnRole string) extension.TurnBudget {
	f.lastCall.spawnSkill = spawnSkill
	f.lastCall.spawnRole = spawnRole
	return f.budget
}

// TestResolveTurnBudget_LookupWins exercises layer 1 — the
// TurnBudgetLookup return overrides every downstream layer. Phase
// 5.2 δ.
func TestResolveTurnBudget_LookupWins(t *testing.T) {
	off := false
	ext := &fakeTurnBudgetExt{budget: extension.TurnBudget{
		SoftCap:        7,
		HardCeiling:    14,
		StuckDetection: &extension.StuckDetectionPolicy{Enabled: &off},
	}}
	s := &Session{
		spawnSkill: "data-chat",
		spawnRole:  "fast",
		deps: &Deps{
			Extensions: []extension.Extension{ext},
			TierDefaults: map[string]TierTurnDefaults{
				"worker": {MaxToolTurns: 40, MaxToolTurnsHard: 80},
			},
		},
		depth: 2, // worker tier
	}
	soft, hard, disabled := s.resolveTurnBudget(context.Background())
	if soft != 7 || hard != 14 || !disabled {
		t.Errorf("got (%d, %d, disabled=%v); want (7, 14, true)", soft, hard, disabled)
	}
	if ext.lastCall.spawnSkill != "data-chat" || ext.lastCall.spawnRole != "fast" {
		t.Errorf("lookup forwarded wrong identity: %+v", ext.lastCall)
	}
}

// TestResolveTurnBudget_TierFallback covers layer 2 — when the
// lookup abstains the operator-supplied tier default wins.
func TestResolveTurnBudget_TierFallback(t *testing.T) {
	on := true
	s := &Session{
		spawnSkill: "data-chat",
		depth:      1, // mission tier
		deps: &Deps{
			Extensions: []extension.Extension{&fakeTurnBudgetExt{}}, // empty budget
			TierDefaults: map[string]TierTurnDefaults{
				"mission": {
					MaxToolTurns:     16,
					MaxToolTurnsHard: 32,
					StuckPolicy:      StuckDetectionDefault{Enabled: &on},
				},
			},
		},
	}
	soft, hard, disabled := s.resolveTurnBudget(context.Background())
	if soft != 16 || hard != 32 {
		t.Errorf("got (%d, %d); want (16, 32) from tier defaults", soft, hard)
	}
	if disabled {
		t.Error("stuck disabled = true; want false (tier Enabled=&true)")
	}
}

// TestResolveTurnBudget_RuntimeFallback covers the bottom of the
// chain: no extension, no tier defaults, no session overrides —
// runtime constants kick in.
func TestResolveTurnBudget_RuntimeFallback(t *testing.T) {
	s := &Session{}
	soft, hard, disabled := s.resolveTurnBudget(context.Background())
	if soft != defaultMaxToolIterations {
		t.Errorf("softCap = %d; want runtime default %d", soft, defaultMaxToolIterations)
	}
	if hard != 0 {
		// hard stays 0 so resolveHardCeiling applies the 2×softCap
		// rule with the caller-supplied softCap argument.
		t.Errorf("hardCeiling = %d; want 0 (chain abstains, resolveHardCeiling applies fallback)", hard)
	}
	if disabled {
		t.Error("stuck disabled = true on empty chain; want false")
	}
}

// TestResolveTurnBudget_FieldIndependence covers the
// "each field independently resolved" semantic across layers:
// lookup supplies softCap only, tier supplies hard ceiling, no
// layer supplies stuck policy ⇒ default-on.
func TestResolveTurnBudget_FieldIndependence(t *testing.T) {
	ext := &fakeTurnBudgetExt{budget: extension.TurnBudget{SoftCap: 5}}
	s := &Session{
		depth: 2,
		deps: &Deps{
			Extensions: []extension.Extension{ext},
			TierDefaults: map[string]TierTurnDefaults{
				"worker": {MaxToolTurns: 40, MaxToolTurnsHard: 80},
			},
		},
	}
	soft, hard, disabled := s.resolveTurnBudget(context.Background())
	if soft != 5 {
		t.Errorf("soft = %d; want 5 (lookup)", soft)
	}
	if hard != 80 {
		t.Errorf("hard = %d; want 80 (tier fallback for hard only)", hard)
	}
	if disabled {
		t.Error("stuck disabled = true; want false (no layer set policy)")
	}
}

// TestResolveTurnBudget_SessionOverride covers layer 4 — when
// every higher layer abstains the WithMaxToolIterations option
// supplies the cap.
func TestResolveTurnBudget_SessionOverride(t *testing.T) {
	s := &Session{
		maxToolIters:     11,
		maxToolItersHard: 22,
	}
	soft, hard, _ := s.resolveTurnBudget(context.Background())
	if soft != 11 || hard != 22 {
		t.Errorf("got (%d, %d); want (11, 22) from session overrides", soft, hard)
	}
}

// TestFirstNonZero pins the helper used by resolveTurnBudget.
func TestFirstNonZero(t *testing.T) {
	cases := []struct {
		in   []int
		want int
	}{
		{nil, 0},
		{[]int{0, 0, 0}, 0},
		{[]int{0, 5, 7}, 5},
		{[]int{3, 0, 9}, 3},
		{[]int{0, 0, 9}, 9},
	}
	for _, tc := range cases {
		if got := firstNonZero(tc.in...); got != tc.want {
			t.Errorf("firstNonZero(%v) = %d; want %d", tc.in, got, tc.want)
		}
	}
}

// TestResolveStuckDisabled pins the per-layer "is stuck detection
// off?" fold.
func TestResolveStuckDisabled(t *testing.T) {
	on := true
	off := false
	cases := []struct {
		name   string
		lookup *extension.StuckDetectionPolicy
		tier   StuckDetectionDefault
		want   bool
	}{
		{"all_abstain_default_on", nil, StuckDetectionDefault{}, false},
		{"lookup_disabled_overrides_tier_on", &extension.StuckDetectionPolicy{Enabled: &off}, StuckDetectionDefault{Enabled: &on}, true},
		{"lookup_enabled_overrides_tier_off", &extension.StuckDetectionPolicy{Enabled: &on}, StuckDetectionDefault{Enabled: &off}, false},
		{"tier_disabled_when_no_lookup", nil, StuckDetectionDefault{Enabled: &off}, true},
		{"tier_enabled_when_no_lookup", nil, StuckDetectionDefault{Enabled: &on}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveStuckDisabled(tc.lookup, tc.tier); got != tc.want {
				t.Errorf("resolveStuckDisabled = %v; want %v", got, tc.want)
			}
		})
	}
}
