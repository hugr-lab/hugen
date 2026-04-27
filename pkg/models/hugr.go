package models

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"log/slog"
	"strings"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/hugr-lab/query-engine/types"
	"google.golang.org/adk/model"
	"google.golang.org/genai"
)

// chatCompletionSubscription is the GraphQL subscription for streaming Hugr LLM calls.
// Returns Arrow RecordBatches with stream events (content_delta, reasoning, tool_use, finish).
const chatCompletionSubscription = `subscription($model: String!, $messages: [String!]!, $max_tokens: Int, $temperature: Float, $tools: [String!], $tool_choice: String) {
	core {
		models {
			chat_completion(
				model: $model,
				messages: $messages,
				max_tokens: $max_tokens,
				temperature: $temperature,
				tools: $tools,
				tool_choice: $tool_choice
			) {
				type
				content
				model
				finish_reason
				tool_calls
				prompt_tokens
				completion_tokens
				thought_signature
			}
		}
	}
}`

// HugrModel implements the ADK model.LLM interface using Hugr GraphQL subscriptions.
// LLM responses stream via WebSocket as Arrow IPC RecordBatches.
type HugrModel struct {
	name           string
	hugrModel      string
	querier        types.Querier
	logger         *slog.Logger
	maxTokens      int           // default max completion tokens (0 = provider default)
	temperature    *float32      // default temperature (nil = provider default)
	toolChoiceFunc func() string // returns "auto" or "required"; nil defaults to "auto"
}

// Option configures a HugrModel.
type Option func(*HugrModel)

// WithLogger sets the logger for the model.
func WithLogger(l *slog.Logger) Option {
	return func(m *HugrModel) { m.logger = l }
}

// WithName sets the ADK model name.
func WithName(name string) Option {
	return func(m *HugrModel) { m.name = name }
}

// WithMaxTokens sets the default max completion tokens per LLM call.
func WithMaxTokens(n int) Option {
	return func(m *HugrModel) { m.maxTokens = n }
}

// WithTemperature sets the default temperature for LLM calls.
// Overrides the server-side default. Can be overridden by ADK request config.
func WithTemperature(t float32) Option {
	return func(m *HugrModel) { m.temperature = &t }
}

// WithToolChoiceFunc sets a dynamic tool_choice provider.
// The function is called on each LLM request to determine tool_choice value.
// Returns "auto" or "required". If nil, defaults to "auto".
func WithToolChoiceFunc(f func() string) Option {
	return func(m *HugrModel) { m.toolChoiceFunc = f }
}

// NewHugr creates a new HugrModel.
//   - q: Hugr querier (embedded engine or remote client — both satisfy types.Querier)
//   - hugrModel: Hugr data source name (e.g. "gemma4-26b")
func NewHugr(q types.Querier, hugrModel string, opts ...Option) *HugrModel {
	m := &HugrModel{
		name:      "hugr-model",
		hugrModel: hugrModel,
		querier:   q,
		logger:    slog.Default(),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Name returns the model identifier for ADK.
func (m *HugrModel) Name() string {
	return m.name
}

// GenerateContent implements model.LLM. It subscribes to Hugr's
// chat_completion stream via WebSocket and yields ADK LLMResponses
// as Arrow IPC RecordBatches arrive with streaming events.
func (m *HugrModel) GenerateContent(
	ctx context.Context,
	req *model.LLMRequest,
	stream bool,
) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		messages, err := adkToHugrMessages(req.Contents)
		if err != nil {
			yield(nil, fmt.Errorf("hugrmodel: convert messages: %w", err))
			return
		}

		vars := map[string]any{
			"model":    m.hugrModel,
			"messages": messages,
		}

		// Defaults from model config, overridden by ADK request.
		if m.maxTokens > 0 {
			vars["max_tokens"] = m.maxTokens
		}
		if m.temperature != nil {
			vars["temperature"] = *m.temperature
		}

		if req.Config != nil {
			if req.Config.MaxOutputTokens != 0 {
				vars["max_tokens"] = req.Config.MaxOutputTokens
			}
			if req.Config.Temperature != nil {
				vars["temperature"] = *req.Config.Temperature
			}
		}

		if req.Config != nil && len(req.Config.Tools) > 0 {
			tools, err := adkToHugrTools(req.Config.Tools)
			if err != nil {
				yield(nil, fmt.Errorf("hugrmodel: convert tools: %w", err))
				return
			}
			if len(tools) > 0 {
				vars["tools"] = tools
				toolChoice := "auto"
				if m.toolChoiceFunc != nil {
					toolChoice = m.toolChoiceFunc()
				}
				vars["tool_choice"] = toolChoice
			}
			m.logger.Debug("hugr tools converted", "count", len(tools))
		}

		m.logger.Debug("hugr chat_completion subscription",
			"model", m.hugrModel,
			"messages_count", len(messages),
		)

		sub, err := m.querier.Subscribe(ctx, chatCompletionSubscription, vars)
		if err != nil {
			m.logger.Error("hugr chat_completion subscribe failed",
				"model", m.hugrModel, "err", err)
			yield(nil, fmt.Errorf("hugrmodel: subscribe: %w", err))
			return
		}
		defer sub.Cancel()

		var (
			fullContent  strings.Builder
			allToolCalls []types.LLMToolCall
			finishEvent  types.LLMStreamEvent
		)

		// Hugr LLM subscriptions return events with an empty path.
		const completionPath = ""
		out := make(chan *model.LLMResponse)
		readCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		go func() {
			defer close(out)
			err = ReadSubscription(readCtx, sub, map[string]BatchHandler{
				completionPath: func(ctx context.Context, batch arrow.RecordBatch) error {
					schema := batch.Schema()
					for i := 0; i < int(batch.NumRows()); i++ {
						select {
						case <-ctx.Done():
							return ctx.Err()
						default:
						}
						ev := readStreamEvent(schema, batch, i)

						switch ev.Type {
						case "content_delta":
							fullContent.WriteString(ev.Content)
							if stream {
								select {
								case <-ctx.Done():
									return ctx.Err()
								case out <- &model.LLMResponse{
									Content: &genai.Content{
										Role:  "model",
										Parts: []*genai.Part{{Text: ev.Content}},
									},
									Partial: true,
								}:
								}
							}
						case "reasoning":
							if ev.Content != "" && stream {
								select {
								case <-ctx.Done():
									return ctx.Err()
								case out <- &model.LLMResponse{
									Content: &genai.Content{
										Role:  "model",
										Parts: []*genai.Part{{Text: ev.Content, Thought: true}},
									},
									Partial: true,
								}:
								}
							}
						case "tool_use":
							if ev.ToolCalls != "" {
								tc, err := parseToolCalls(ev.ToolCalls)
								if err != nil {
									return fmt.Errorf("parse tool_use: %w", err)
								}
								allToolCalls = append(allToolCalls, tc...)
							}

						case "finish":
							finishEvent = ev
							if ev.ToolCalls != "" {
								tc, err := parseToolCalls(ev.ToolCalls)
								if err != nil {
									return fmt.Errorf("parse finish tool_calls: %w", err)
								}
								allToolCalls = append(allToolCalls, tc...)
							}

						case "error":
							return fmt.Errorf("stream error: %s", ev.Content)
						}
					}
					return nil
				},
			})
		}()
		for item := range out {
			if item != nil {
				if !yield(item, nil) {
					return
				}
			}
		}
		if err != nil {
			m.logger.Error("hugr chat_completion subscription failed",
				"model", m.hugrModel, "err", err)
			yield(nil, fmt.Errorf("hugrmodel: subscription: %w", err))
			return
		}

		if finishEvent.Model == "" && fullContent.Len() == 0 && len(allToolCalls) == 0 {
			m.logger.Error("hugr chat_completion empty response",
				"model", m.hugrModel,
				"messages_count", len(messages))
			yield(nil, fmt.Errorf("hugrmodel: empty response from LLM — provider may have returned an error (rate limit, invalid request). Check Hugr server logs"))
			return
		}

		m.logger.Info("hugr completion",
			"model", finishEvent.Model,
			"finish_reason", finishEvent.FinishReason,
			"prompt_tokens", finishEvent.PromptTokens,
			"completion_tokens", finishEvent.CompletionTokens,
			"tool_calls", len(allToolCalls),
		)

		// Final TurnComplete response with full accumulated content + tool calls.
		result := types.LLMResult{
			Content:          fullContent.String(),
			Model:            finishEvent.Model,
			FinishReason:     finishEvent.FinishReason,
			PromptTokens:     finishEvent.PromptTokens,
			CompletionTokens: finishEvent.CompletionTokens,
			TotalTokens:      finishEvent.PromptTokens + finishEvent.CompletionTokens,
			ToolCalls:        allToolCalls,
			ThoughtSignature: finishEvent.ThoughtSignature,
		}

		yield(&model.LLMResponse{
			Content:      hugrResultToADKContent(result),
			FinishReason: mapFinishReason(result.FinishReason),
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:     int32(result.PromptTokens),
				CandidatesTokenCount: int32(result.CompletionTokens),
				TotalTokenCount:      int32(result.TotalTokens),
			},
			TurnComplete: true,
		}, nil)
	}
}

// readStreamEvent extracts a types.LLMStreamEvent from an Arrow RecordBatch row.
func readStreamEvent(schema *arrow.Schema, batch arrow.RecordBatch, rowIdx int) types.LLMStreamEvent {
	var ev types.LLMStreamEvent
	for j := 0; j < int(batch.NumCols()); j++ {
		val := batch.Column(j).GetOneForMarshal(rowIdx)
		switch schema.Field(j).Name {
		case "type":
			ev.Type = stringVal(val)
		case "content":
			ev.Content = stringVal(val)
		case "model":
			ev.Model = stringVal(val)
		case "finish_reason":
			ev.FinishReason = stringVal(val)
		case "tool_calls":
			ev.ToolCalls = stringVal(val)
		case "prompt_tokens":
			ev.PromptTokens = intVal(val)
		case "completion_tokens":
			ev.CompletionTokens = intVal(val)
		case "thought_signature":
			ev.ThoughtSignature = stringVal(val)
		}
	}
	return ev
}

func stringVal(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func intVal(v any) int {
	if v == nil {
		return 0
	}
	switch n := v.(type) {
	case int:
		return n
	case int32:
		return int(n)
	case int64:
		return int(n)
	case float32:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

// parseToolCalls parses tool calls from the Hugr stream ToolCalls string.
//
// Hugr stream format depends on the provider:
//   - OpenAI (streaming): raw concatenated JSON arguments string, e.g. `{"query":"test"}`.
//     Name and ID are lost during streaming (query-engine accumulates only argument fragments).
//   - Non-streaming (all providers): `[{"id":"...","name":"...","arguments":{...}}]`
//   - Anthropic/Gemini streaming: tool calls not yet sent in stream events.
//
// This function handles all variants.
func parseToolCalls(raw string) ([]types.LLMToolCall, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	truncated := raw
	if len(truncated) > 200 {
		truncated = truncated[:200] + "..."
	}

	switch {
	case strings.HasPrefix(raw, "["):
		var calls []types.LLMToolCall
		if err := json.Unmarshal([]byte(raw), &calls); err != nil {
			return nil, fmt.Errorf("unmarshal tool_calls array: %w (raw: %s)", err, truncated)
		}
		return calls, nil

	case strings.HasPrefix(raw, "{"):
		var tc types.LLMToolCall
		if err := json.Unmarshal([]byte(raw), &tc); err != nil {
			return nil, fmt.Errorf("unmarshal tool_call object: %w (raw: %s)", err, truncated)
		}
		if tc.Name != "" {
			return []types.LLMToolCall{tc}, nil
		}
		// Raw arguments without name/id wrapper (OpenAI streaming via Hugr).
		var args any
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			return nil, fmt.Errorf("unmarshal tool_call args: %w (raw: %s)", err, truncated)
		}
		return []types.LLMToolCall{{Arguments: args}}, nil

	default:
		return nil, fmt.Errorf("unexpected tool_calls format (raw: %s)", truncated)
	}
}
