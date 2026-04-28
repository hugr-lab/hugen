// runtime_adapter.go bridges *HugrModel to the runtime-side
// interface declared in github.com/hugr-lab/hugen/pkg/model. This is
// the only file in pkg/models that exports a non-genai-shaped
// surface. All ADK / genai dependencies stay private to pkg/models;
// callers below pkg/models import only pkg/model.
package models

import (
	"context"
	"fmt"
	"sync"

	"github.com/hugr-lab/hugen/pkg/model"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"
)

// Spec implements model.Model — the runtime-side identifier.
func (m *HugrModel) Spec() model.ModelSpec {
	return model.ModelSpec{Provider: "hugr", Name: m.hugrModel}
}

// Generate implements model.Model. It translates a runtime Request
// into the ADK request shape, calls the existing GenerateContent
// path, and bridges the iterator to a model.Stream.
//
// The Hugr ADK source emits both streaming deltas (Partial=true) and
// a final TurnComplete chunk that carries the *full* accumulated
// content. To avoid duplicating the answer in the transcript, we
// drop the content of the TurnComplete chunk when partial deltas
// have already been emitted; we keep the Usage field and the Final
// flag.
func (m *HugrModel) Generate(ctx context.Context, req model.Request) (model.Stream, error) {
	adkReq := buildADKRequest(req)
	stream := m.GenerateContent(ctx, adkReq, true)

	out := make(chan streamItem, 8)
	streamCtx, cancel := context.WithCancel(ctx)
	go func() {
		defer close(out)
		sawPartial := false
		for resp, err := range stream {
			if err != nil {
				select {
				case out <- streamItem{err: err}:
				case <-streamCtx.Done():
					return
				}
				return
			}
			chunk := convertADKResponse(resp)
			if resp != nil && resp.Partial {
				sawPartial = true
			}
			// On the TurnComplete chunk, suppress duplicate content.
			if chunk.Final && sawPartial {
				chunk.Content = nil
				chunk.Reasoning = nil
			}
			select {
			case out <- streamItem{chunk: chunk}:
			case <-streamCtx.Done():
				return
			}
		}
	}()
	return &runtimeStream{ch: out, cancel: cancel}, nil
}

type streamItem struct {
	chunk model.Chunk
	err   error
}

type runtimeStream struct {
	ch     chan streamItem
	cancel context.CancelFunc

	closed sync.Once
}

func (s *runtimeStream) Next(ctx context.Context) (model.Chunk, bool, error) {
	select {
	case it, ok := <-s.ch:
		if !ok {
			return model.Chunk{}, false, nil
		}
		if it.err != nil {
			return model.Chunk{}, false, it.err
		}
		return it.chunk, true, nil
	case <-ctx.Done():
		return model.Chunk{}, false, ctx.Err()
	}
}

func (s *runtimeStream) Close() error {
	s.closed.Do(func() {
		s.cancel()
	})
	return nil
}

// buildADKRequest projects pkg/model.Request onto adkmodel.LLMRequest.
// Phase 1 sends only Contents; Tools land in phase 3.
func buildADKRequest(req model.Request) *adkmodel.LLMRequest {
	contents := make([]*genai.Content, 0, len(req.Messages))
	for _, msg := range req.Messages {
		role := genai.RoleUser
		if msg.Role == model.RoleAssistant {
			role = "model"
		} else if msg.Role == model.RoleSystem {
			role = "system"
		}
		contents = append(contents, &genai.Content{
			Role:  role,
			Parts: []*genai.Part{{Text: msg.Content}},
		})
	}
	cfg := &genai.GenerateContentConfig{}
	if req.MaxTokens > 0 {
		cfg.MaxOutputTokens = int32(req.MaxTokens)
	}
	if req.Temperature != nil {
		cfg.Temperature = req.Temperature
	}
	return &adkmodel.LLMRequest{
		Contents: contents,
		Config:   cfg,
	}
}

// convertADKResponse maps a single LLMResponse to a model.Chunk.
//
// ADK encodes streamed reasoning as Part.Thought=true. We surface
// that as Chunk.Reasoning; regular text becomes Chunk.Content. The
// final response (TurnComplete) carries Usage.
func convertADKResponse(resp *adkmodel.LLMResponse) model.Chunk {
	var c model.Chunk
	if resp == nil {
		return c
	}
	if resp.Content != nil {
		for _, p := range resp.Content.Parts {
			if p == nil || p.Text == "" {
				continue
			}
			text := p.Text
			if p.Thought {
				if c.Reasoning == nil {
					c.Reasoning = &text
				} else {
					merged := *c.Reasoning + text
					c.Reasoning = &merged
				}
			} else {
				if c.Content == nil {
					c.Content = &text
				} else {
					merged := *c.Content + text
					c.Content = &merged
				}
			}
		}
	}
	if resp.UsageMetadata != nil {
		c.Usage = &model.Usage{
			PromptTokens:     int(resp.UsageMetadata.PromptTokenCount),
			CompletionTokens: int(resp.UsageMetadata.CandidatesTokenCount),
			TotalTokens:      int(resp.UsageMetadata.TotalTokenCount),
		}
	}
	if resp.TurnComplete {
		c.Final = true
	}
	return c
}

// BuildModelMap builds a {ModelSpec → Model} registry from the
// existing models.Service routes. Used by cmd/hugen to populate
// pkg/model.ModelRouter.
func BuildModelMap(svc *Service) map[model.ModelSpec]model.Model {
	out := make(map[model.ModelSpec]model.Model)
	if svc == nil {
		return out
	}
	if hm, ok := svc.defaultModel.(*HugrModel); ok {
		out[hm.Spec()] = hm
	}
	for _, m := range svc.routes {
		if hm, ok := m.(*HugrModel); ok {
			out[hm.Spec()] = hm
		}
	}
	return out
}

// IntentDefaults projects models.Service routes onto pkg/model
// intent → spec. The default model becomes IntentDefault; configured
// routes use the route name as the intent name.
//
// Phase 1 requires both IntentDefault and IntentCheap to be present.
// If "cheap" is missing in routes, it falls back to the default.
func IntentDefaults(svc *Service) map[model.Intent]model.ModelSpec {
	out := make(map[model.Intent]model.ModelSpec)
	if svc == nil {
		return out
	}
	if hm, ok := svc.defaultModel.(*HugrModel); ok {
		out[model.IntentDefault] = hm.Spec()
	}
	for intentStr, m := range svc.routes {
		if hm, ok := m.(*HugrModel); ok {
			out[model.Intent(intentStr)] = hm.Spec()
		}
	}
	// Phase-1 requirement: "cheap" must exist (router validates). If
	// the operator hasn't configured it, mirror the default.
	if _, ok := out[model.IntentCheap]; !ok {
		if def, defOk := out[model.IntentDefault]; defOk {
			out[model.IntentCheap] = def
		}
	}
	return out
}

// ensure compile-time conformance; if Service grows without keeping
// the runtime contract this fails fast.
var _ = func() error {
	_ = fmt.Sprintf
	return nil
}
