package models

import (
	"context"
	"iter"
	"sync"

	"github.com/hugr-lab/query-engine/types"
	"google.golang.org/adk/model"
)

// Intent classifies the current LLM task for model routing.
type Intent string

const (
	// IntentDefault is for general reasoning and user interaction.
	IntentDefault Intent = "default"

	// IntentToolCalling is for tool selection and execution (cheap model for sub-agents).
	IntentToolCalling Intent = "tool_calling"

	// IntentSummarization is for context compaction and summaries (cheap model).
	IntentSummarization Intent = "summarization"

	// IntentClassification is for intent/category detection (cheap model).
	IntentClassification Intent = "classification"
)

type Service struct {
	config Config

	local, remote types.Querier

	mu           sync.RWMutex
	defaultModel model.LLM
	routes       map[Intent]model.LLM
}

func New(ctx context.Context, local, remote types.Querier, config Config, opts ...Option) *Service {

	routes := make(map[Intent]model.LLM)
	for intentStr, cfg := range config.Routes {
		intent := Intent(intentStr)
		if cfg.Mode == LocalMode {
			routes[intent] = NewHugr(local, cfg.Model, append(opts, cfg.BuildOpts()...)...)
		} else {
			routes[intent] = NewHugr(remote, cfg.Model, append(opts, cfg.BuildOpts()...)...)
		}
	}
	var defaultModel model.LLM
	if config.Mode == LocalMode {
		defaultModel = NewHugr(local, config.Model, opts...)
	} else {
		defaultModel = NewHugr(remote, config.Model, opts...)
	}

	return &Service{
		config:       config,
		local:        local,
		remote:       remote,
		routes:       routes,
		defaultModel: defaultModel,
	}
}

// model.LLM interface — delegates via ModelFor(IntentDefault).
func (r *Service) Name() string {
	return r.ModelFor(IntentDefault).Name()
}

func (r *Service) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return r.ModelFor(IntentDefault).GenerateContent(ctx, req, stream)
}

// ModelFor returns the model mapped to the given intent, falling back
// to the default model when no explicit route is set.
func (r *Service) ModelFor(intent Intent) model.LLM {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if m, ok := r.routes[intent]; ok {
		return m
	}
	return r.defaultModel
}
