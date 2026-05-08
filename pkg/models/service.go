package models

import (
	"context"
	"sync"

	"github.com/hugr-lab/query-engine/types"

	"github.com/hugr-lab/hugen/pkg/config"
	"github.com/hugr-lab/hugen/pkg/model"
)

// Intent classifies the current LLM task for model routing.
type Intent string

const (
	IntentDefault        Intent = "default"
	IntentToolCalling    Intent = "tool_calling"
	IntentSummarization  Intent = "summarization"
	IntentClassification Intent = "classification"
)

// Service is the per-process registry of *HugrModel instances keyed
// by intent name. Holds a ModelsView and rebuilds its route map
// when the view fires OnUpdate (phase 6+ live reload).
type Service struct {
	view config.ModelsView
	opts []Option

	local, remote types.Querier

	mu           sync.RWMutex
	defaultModel model.Model
	routes       map[Intent]model.Model

	cancelUpdate func()
}

// New constructs the registry from a ModelsView. The initial route
// table is built immediately; if the view ever fires OnUpdate
// (phase 6+ live reload), Service rebuilds in place and any new
// resolution sees the updated route. The Stop method releases
// the OnUpdate subscription.
func New(ctx context.Context, local, remote types.Querier, view config.ModelsView, opts ...Option) *Service {
	_ = ctx // reserved for future ctx-aware initialisation
	s := &Service{
		view:   view,
		opts:   opts,
		local:  local,
		remote: remote,
	}
	s.rebuild()
	if view != nil {
		s.cancelUpdate = view.OnUpdate(s.rebuild)
	}
	return s
}

// Stop detaches the view subscription. Idempotent.
func (s *Service) Stop() {
	if s.cancelUpdate != nil {
		s.cancelUpdate()
		s.cancelUpdate = nil
	}
}

func (s *Service) rebuild() {
	cfg := config.ModelsConfig{}
	if s.view != nil {
		cfg = s.view.ModelsConfig()
	}
	routes := make(map[Intent]model.Model, len(cfg.Routes))
	for intentStr, route := range cfg.Routes {
		routes[Intent(intentStr)] = newRouteModel(s.local, s.remote, route, s.opts)
	}
	s.mu.Lock()
	s.defaultModel = newRouteModel(s.local, s.remote, cfg, s.opts)
	s.routes = routes
	s.mu.Unlock()
}

func newRouteModel(local, remote types.Querier, cfg config.ModelsConfig, opts []Option) model.Model {
	target := remote
	if cfg.Mode == config.ModeLocal {
		target = local
	}
	return NewHugr(target, cfg.Model, append(opts, buildOptsFor(cfg)...)...)
}

// buildOptsFor lifts MaxTokens / Temperature / retry knobs off a
// ModelsConfig into the per-call Option slice. Lives in pkg/models
// because Option is a pkg/models concept; the config struct itself
// stays a pure data shape in pkg/config.
//
// Retry: zero / negative RetryMaxAttempts falls back to
// DefaultRetryMaxAttempts (10) so the operator gets transient-
// error resilience by default. Set retry_max_attempts: 0 explicitly
// in YAML when retries should be disabled — viper / mapstructure
// distinguishes the unset case from the explicit-zero case via
// the same int(0) value, so the only way to disable today is a
// negative number (-1). Acceptable for v1; revisit when an
// observed need shows up.
func buildOptsFor(cfg config.ModelsConfig) []Option {
	var out []Option
	if cfg.MaxTokens > 0 {
		out = append(out, WithMaxTokens(cfg.MaxTokens))
	}
	if cfg.Temperature > 0 {
		out = append(out, WithTemperature(cfg.Temperature))
	}
	maxAttempts := cfg.RetryMaxAttempts
	if maxAttempts == 0 {
		maxAttempts = config.DefaultRetryMaxAttempts
	} else if maxAttempts < 0 {
		maxAttempts = 0
	}
	initialBackoff := cfg.RetryInitialBackoff
	if initialBackoff <= 0 {
		initialBackoff = config.DefaultRetryInitialBackoff
	}
	out = append(out, WithRetry(maxAttempts, initialBackoff))
	return out
}

// ModelFor returns the model registered for the given intent,
// falling back to the default model.
func (s *Service) ModelFor(intent Intent) model.Model {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if m, ok := s.routes[intent]; ok {
		return m
	}
	return s.defaultModel
}

// BuildModelMap projects a Service's routes onto a {ModelSpec → Model}
// registry, suitable for handing to model.NewModelRouter.
func BuildModelMap(svc *Service) map[model.ModelSpec]model.Model {
	out := make(map[model.ModelSpec]model.Model)
	if svc == nil {
		return out
	}
	if svc.defaultModel != nil {
		out[svc.defaultModel.Spec()] = svc.defaultModel
	}
	for _, m := range svc.routes {
		if m == nil {
			continue
		}
		out[m.Spec()] = m
	}
	return out
}

// IntentDefaults projects a Service's routes onto an intent → spec
// map. The default model becomes IntentDefault. If the operator
// hasn't configured "cheap", it mirrors the default (phase-1 router
// requires both intents to be present).
func IntentDefaults(svc *Service) map[model.Intent]model.ModelSpec {
	out := make(map[model.Intent]model.ModelSpec)
	if svc == nil {
		return out
	}
	if svc.defaultModel != nil {
		out[model.IntentDefault] = svc.defaultModel.Spec()
	}
	for intentStr, m := range svc.routes {
		if m == nil {
			continue
		}
		out[model.Intent(intentStr)] = m.Spec()
	}
	if _, ok := out[model.IntentCheap]; !ok {
		if def, defOk := out[model.IntentDefault]; defOk {
			out[model.IntentCheap] = def
		}
	}
	return out
}
