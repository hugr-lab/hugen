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
}

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

func (r *ModelRouter) lookup(spec ModelSpec) (Model, ModelSpec, error) {
	r.mu.RLock()
	m, ok := r.models[spec]
	r.mu.RUnlock()
	if !ok {
		return nil, spec, fmt.Errorf("%w: spec %s not registered", ErrModelUnavailable, spec)
	}
	return m, spec, nil
}
