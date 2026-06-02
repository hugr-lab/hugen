package model

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrModelUnavailable is returned by Resolve when no step in the
// 5-step lookup yields a registered model.
var ErrModelUnavailable = errors.New("model: unavailable")

// ErrModelMisconfigured is returned by NewModelRouter when the
// runtime defaults are missing required intents or reference a
// ModelSpec that has no concrete provider registered.
var ErrModelMisconfigured = errors.New("model: misconfigured")

// ModelRouter resolves a model for a given Hint via deterministic
// 5-step lookup. See contracts/model-router.md.
type ModelRouter struct {
	mu       sync.RWMutex
	defaults map[Intent]ModelSpec
	models   map[ModelSpec]Model

	// Context-budget metadata (Phase 5.2 budget-termination), wired
	// once at boot via [ModelRouter.SetContextBudgets]. Zero/nil maps
	// are tolerated — the accessors fall back to the floor / default
	// ratio so a router built without budgets stays correct.
	budgets       map[ModelSpec]int  // input-context capacity per model (tokens)
	defaultBudget int                // fallback capacity when a spec has none
	ratios        map[Intent]float64 // per-intent soft context-budget ratio
	intentBudgets map[Intent]int     // per-intent budget override (routes.<intent>.default_budget)
}

// DefaultContextBudgetRatio is the single fraction of the configured
// context budget at which the runtime blocks further tool work and
// makes a subagent summarise + hand off (budget-termination). The 0.85
// default leaves headroom for the finalize turn before the real context
// limit. Phase 5.2.
const DefaultContextBudgetRatio = 0.85

// NewModelRouter validates that defaults["default"] is set and that
// every ModelSpec listed in defaults is present in models.
func NewModelRouter(defaults map[Intent]ModelSpec, models map[ModelSpec]Model) (*ModelRouter, error) {
	if _, ok := defaults[IntentDefault]; !ok {
		return nil, fmt.Errorf("%w: defaults[%q] missing", ErrModelMisconfigured, IntentDefault)
	}
	for intent, spec := range defaults {
		if _, ok := models[spec]; !ok {
			return nil, fmt.Errorf("%w: intent %q -> %s, no provider registered", ErrModelMisconfigured, intent, spec)
		}
	}
	cd := make(map[Intent]ModelSpec, len(defaults))
	for k, v := range defaults {
		cd[k] = v
	}
	cm := make(map[ModelSpec]Model, len(models))
	for k, v := range models {
		cm[k] = v
	}
	return &ModelRouter{defaults: cd, models: cm}, nil
}

// Resolve runs the 5-step lookup. Returns the chosen Model and the
// resolved ModelSpec.
func (r *ModelRouter) Resolve(ctx context.Context, hint Hint) (Model, ModelSpec, error) {
	intent := hint.Intent
	if intent == "" {
		intent = IntentDefault
	}

	// Step 1: explicit override.
	if hint.ModelOverride != nil {
		return r.lookup(*hint.ModelOverride)
	}
	// Step 2: per-session intent map.
	if spec, ok := hint.SessionModels[intent]; ok {
		return r.lookup(spec)
	}
	// Step 3: active-skill intent map.
	if spec, ok := hint.SkillModels[intent]; ok {
		return r.lookup(spec)
	}
	// Step 4: runtime default for this intent.
	r.mu.RLock()
	spec, ok := r.defaults[intent]
	r.mu.RUnlock()
	if ok {
		return r.lookup(spec)
	}
	// Step 5: terminal fallback to default intent.
	r.mu.RLock()
	spec, ok = r.defaults[IntentDefault]
	r.mu.RUnlock()
	if ok {
		return r.lookup(spec)
	}
	return nil, ModelSpec{}, fmt.Errorf("%w: no model for intent %q", ErrModelUnavailable, intent)
}

// Defaults returns a copy of the runtime defaults — used by callers
// that need to materialise a ModelSpec from an intent name (e.g.
// /model use cheap).
func (r *ModelRouter) Defaults() map[Intent]ModelSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[Intent]ModelSpec, len(r.defaults))
	for k, v := range r.defaults {
		out[k] = v
	}
	return out
}

// SpecFor returns the runtime default spec for intent (no fallback,
// no per-session/skill consideration).
func (r *ModelRouter) SpecFor(intent Intent) (ModelSpec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.defaults[intent]
	return s, ok
}

// Has returns true if a Model is registered for the given spec.
func (r *ModelRouter) Has(spec ModelSpec) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.models[spec]
	return ok
}

// SetContextBudgets wires the per-model context windows, per-intent
// soft ratios, and per-intent budget overrides resolved from operator
// config. Called once at boot, after [NewModelRouter]. nil maps are
// tolerated.
func (r *ModelRouter) SetContextBudgets(budgets map[ModelSpec]int, defaultBudget int, ratios map[Intent]float64, intentBudgets map[Intent]int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.budgets = budgets
	r.defaultBudget = defaultBudget
	r.ratios = ratios
	r.intentBudgets = intentBudgets
}

// MaxPromptTokens returns the configured context budget (tokens) for
// intent, or 0 when none is configured — 0 means UNLIMITED: the budget
// guard is OFF for that intent. The feature is fully opt-in; with
// nothing configured the runtime never budget-terminates. Precedence:
// per-intent override (routes.<intent>.default_budget) → the intent's
// model ContextWindow → the global DefaultBudget. The per-intent
// override lets a dedicated worker intent carry a tighter budget than
// the model it shares with the orchestration roles. Resolution uses the
// runtime-default spec for the intent (not per-session / per-skill
// overrides) — the budget is an approximate ceiling.
func (r *ModelRouter) MaxPromptTokens(intent Intent) int {
	if intent == "" {
		intent = IntentDefault
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if b, ok := r.intentBudgets[intent]; ok && b > 0 {
		return b
	}
	spec, ok := r.defaults[intent]
	if !ok {
		spec = r.defaults[IntentDefault]
	}
	if b, ok := r.budgets[spec]; ok && b > 0 {
		return b
	}
	return r.defaultBudget // 0 when unset → unlimited (budget off)
}

// ContextBudgetRatio returns the single budget fraction of
// [ModelRouter.MaxPromptTokens] for intent — the per-call prompt point
// at which the runtime blocks tools + makes the subagent summarise.
// Defaults to [DefaultContextBudgetRatio]; operator-overridable per
// route (models.routes.<intent>.context_budget_ratio).
func (r *ModelRouter) ContextBudgetRatio(intent Intent) float64 {
	if intent == "" {
		intent = IntentDefault
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if v, ok := r.ratios[intent]; ok && v > 0 {
		return v
	}
	return DefaultContextBudgetRatio
}

func (r *ModelRouter) lookup(spec ModelSpec) (Model, ModelSpec, error) {
	r.mu.RLock()
	m, ok := r.models[spec]
	r.mu.RUnlock()
	if !ok {
		return nil, spec, fmt.Errorf("%w: spec %s not registered", ErrModelUnavailable, spec)
	}
	return m, spec, nil
}
