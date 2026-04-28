package models

import (
	"context"
	"sync"

	"github.com/hugr-lab/query-engine/types"

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
// by intent name. Phase 2 (R-Plan-23) keyed it on pkg/model.Model
// instead of the previous ADK adkmodel.LLM type. The shape of
// New / ModelFor / BuildModelMap / IntentDefaults is unchanged.
type Service struct {
	config Config

	local, remote types.Querier

	mu           sync.RWMutex
	defaultModel model.Model
	routes       map[Intent]model.Model
}

func New(ctx context.Context, local, remote types.Querier, cfg Config, opts ...Option) *Service {
	_ = ctx // reserved for future async config refresh
	routes := make(map[Intent]model.Model, len(cfg.Routes))
	for intentStr, route := range cfg.Routes {
		routes[Intent(intentStr)] = newRouteModel(local, remote, route, opts)
	}
	return &Service{
		config:       cfg,
		local:        local,
		remote:       remote,
		routes:       routes,
		defaultModel: newRouteModel(local, remote, cfg, opts),
	}
}

func newRouteModel(local, remote types.Querier, cfg Config, opts []Option) model.Model {
	if cfg.Mode == LocalMode {
		return NewHugr(local, cfg.Model, append(opts, cfg.BuildOpts()...)...)
	}
	return NewHugr(remote, cfg.Model, append(opts, cfg.BuildOpts()...)...)
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
