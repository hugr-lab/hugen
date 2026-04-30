// Package models bridges Hugr's chat_completion GraphQL subscription
// to the runtime-side pkg/model.Model interface.
//
// Phase 2 (R-Plan-23) removed the ADK / genai bridge entirely.
// *HugrModel now implements pkg/model.Model directly: subscription
// over Arrow IPC in, pkg/model.Chunk out. No genai.Content, no
// adkmodel.LLMRequest, no transitive google.golang.org/(adk|genai)
// import in the binary.
package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/hugr-lab/query-engine/types"

	"github.com/hugr-lab/hugen/pkg/model"
)

// chatCompletionSubscription streams chat completions via the Hugr
// engine. The subscription emits Arrow record batches with one row
// per stream event (content_delta / reasoning / tool_use / finish /
// error).
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
				thinking
			}
		}
	}
}`

// HugrModel implements pkg/model.Model against Hugr's GraphQL
// chat_completion subscription. Each Generate opens its own
// subscription; Stream.Close cancels the subscription's context,
// which propagates through the WebSocket so the upstream provider
// observes the cancellation within one network round-trip.
type HugrModel struct {
	name           string
	hugrModel      string
	querier        types.Querier
	logger         *slog.Logger
	maxTokens      int
	temperature    *float32
	toolChoiceFunc func() string
}

// Option configures a HugrModel.
type Option func(*HugrModel)

func WithLogger(l *slog.Logger) Option {
	return func(m *HugrModel) {
		if l != nil {
			m.logger = l
		}
	}
}

func WithName(name string) Option {
	return func(m *HugrModel) { m.name = name }
}

func WithMaxTokens(n int) Option {
	return func(m *HugrModel) { m.maxTokens = n }
}

func WithTemperature(t float32) Option {
	return func(m *HugrModel) { m.temperature = &t }
}

func WithToolChoiceFunc(f func() string) Option {
	return func(m *HugrModel) { m.toolChoiceFunc = f }
}

// NewHugr builds a HugrModel pinned to a specific Hugr LLM data
// source name (e.g. "gemma4-26b").
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

// Name returns the model identifier (used by callers and logs).
func (m *HugrModel) Name() string { return m.name }

// Spec implements model.Model.
func (m *HugrModel) Spec() model.ModelSpec {
	return model.ModelSpec{Provider: "hugr", Name: m.hugrModel}
}

// Generate implements model.Model. Opens a chat_completion
// subscription and returns a Stream that yields Chunks until the
// subscription emits "finish" (or Close is called).
func (m *HugrModel) Generate(ctx context.Context, req model.Request) (model.Stream, error) {
	messages, err := messagesToHugrJSON(req.Messages)
	if err != nil {
		return nil, fmt.Errorf("hugrmodel: convert messages: %w", err)
	}

	vars := map[string]any{
		"model":    m.hugrModel,
		"messages": messages,
	}
	if m.maxTokens > 0 {
		vars["max_tokens"] = m.maxTokens
	}
	if m.temperature != nil {
		vars["temperature"] = *m.temperature
	}
	if req.MaxTokens > 0 {
		vars["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		vars["temperature"] = *req.Temperature
	}
	if len(req.Tools) > 0 {
		tools, err := toolsToHugrJSON(req.Tools)
		if err != nil {
			return nil, fmt.Errorf("hugrmodel: convert tools: %w", err)
		}
		if len(tools) > 0 {
			vars["tools"] = tools
			toolChoice := "auto"
			if m.toolChoiceFunc != nil {
				toolChoice = m.toolChoiceFunc()
			}
			vars["tool_choice"] = toolChoice
		}
	}

	m.logger.Debug("hugr chat_completion subscription",
		"model", m.hugrModel,
		"messages_count", len(messages),
		"tools_count", len(req.Tools),
	)

	subCtx, cancel := context.WithCancel(ctx)
	sub, err := m.querier.Subscribe(subCtx, chatCompletionSubscription, vars)
	if err != nil {
		cancel()
		m.logger.Error("hugr chat_completion subscribe failed",
			"model", m.hugrModel, "err", err)
		return nil, fmt.Errorf("hugrmodel: subscribe: %w", err)
	}

	out := make(chan streamItem, 8)
	go m.pumpSubscription(subCtx, sub, out)
	return &hugrStream{
		ch:     out,
		sub:    sub,
		cancel: cancel,
		logger: m.logger,
		model:  m.hugrModel,
	}, nil
}

// pumpSubscription reads RecordBatches from the Hugr subscription,
// converts each row to a model.Chunk, and pushes onto out. Closes out
// when the subscription ends or ctx is cancelled.
func (m *HugrModel) pumpSubscription(ctx context.Context, sub *types.Subscription, out chan<- streamItem) {
	defer close(out)
	const completionPath = ""
	var finishEv types.LLMStreamEvent
	var sawFinish bool

	err := ReadSubscription(ctx, sub, map[string]BatchHandler{
		completionPath: func(ctx context.Context, batch arrow.RecordBatch) error {
			schema := batch.Schema()
			for i := 0; i < int(batch.NumRows()); i++ {
				if err := ctx.Err(); err != nil {
					return err
				}
				ev := readStreamEvent(schema, batch, i)
				if ev.Type == "error" {
					return fmt.Errorf("stream error: %s", ev.Content)
				}
				if ev.Type == "finish" {
					finishEv = ev
					sawFinish = true
					continue // emitted after the loop with Final=true
				}
				chunk, ok := streamEventToChunk(ev)
				if !ok {
					continue
				}
				if err := sendItem(ctx, out, streamItem{chunk: chunk}); err != nil {
					return err
				}
			}
			return nil
		},
	})
	if err != nil && !isCanceled(err) {
		m.logger.Error("hugr chat_completion subscription failed",
			"model", m.hugrModel, "err", err)
		_ = sendItem(ctx, out, streamItem{err: fmt.Errorf("hugrmodel: subscription: %w", err)})
		return
	}
	if !sawFinish {
		// Subscription closed without a finish event. Treat as
		// canceled / drained — no terminal chunk to emit.
		return
	}
	// Hugr collapses tool_use into the finish event (carries
	// `tool_use` finish_reason + a populated ToolCalls string)
	// rather than streaming a separate tool_use chunk. Emit each
	// tool call as its own chunk BEFORE the terminal Final chunk
	// so the runtime's Turn loop sees them.
	if finishEv.ToolCalls != "" {
		calls, err := parseToolCalls(finishEv.ToolCalls)
		if err != nil {
			m.logger.Warn("hugr completion: parse tool_calls failed",
				"model", finishEv.Model, "err", err, "raw_len", len(finishEv.ToolCalls))
		}
		for _, c := range calls {
			tc := model.ChunkToolCall{
				ID:   c.ID,
				Name: c.Name,
				Args: c.Arguments,
			}
			_ = sendItem(ctx, out, streamItem{chunk: model.Chunk{ToolCall: &tc}})
		}
	}
	final := model.Chunk{
		Final:            true,
		Thinking:         finishEv.Thinking,
		ThoughtSignature: finishEv.ThoughtSignature,
	}
	if finishEv.PromptTokens != 0 || finishEv.CompletionTokens != 0 {
		final.Usage = &model.Usage{
			PromptTokens:     finishEv.PromptTokens,
			CompletionTokens: finishEv.CompletionTokens,
			TotalTokens:      finishEv.PromptTokens + finishEv.CompletionTokens,
		}
	}
	m.logger.Info("hugr completion",
		"model", finishEv.Model,
		"finish_reason", finishEv.FinishReason,
		"prompt_tokens", finishEv.PromptTokens,
		"completion_tokens", finishEv.CompletionTokens,
		"tool_calls_emitted", finishEv.ToolCalls != "",
	)
	_ = sendItem(ctx, out, streamItem{chunk: final})
}

// streamEventToChunk maps a single Hugr stream event onto a
// model.Chunk. Returns (chunk, true) for delta-bearing events;
// returns ok=false for events that carry no chunk content.
func streamEventToChunk(ev types.LLMStreamEvent) (model.Chunk, bool) {
	switch ev.Type {
	case "content_delta":
		if ev.Content == "" {
			return model.Chunk{}, false
		}
		text := ev.Content
		return model.Chunk{Content: &text}, true
	case "reasoning":
		if ev.Content == "" {
			return model.Chunk{}, false
		}
		text := ev.Content
		return model.Chunk{Reasoning: &text}, true
	case "tool_use":
		if ev.ToolCalls == "" {
			return model.Chunk{}, false
		}
		calls, err := parseToolCalls(ev.ToolCalls)
		if err != nil || len(calls) == 0 {
			return model.Chunk{}, false
		}
		first := calls[0]
		return model.Chunk{ToolCall: &model.ChunkToolCall{
			ID:   first.ID,
			Name: first.Name,
			Args: first.Arguments,
		}}, true
	}
	return model.Chunk{}, false
}

func sendItem(ctx context.Context, out chan<- streamItem, it streamItem) error {
	select {
	case out <- it:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func isCanceled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// streamItem carries either a chunk or an error from the pump
// goroutine to the consumer of *hugrStream.
type streamItem struct {
	chunk model.Chunk
	err   error
}

// hugrStream implements model.Stream over a Hugr GraphQL subscription.
//
// Lifecycle contract: callers MUST call Close() exactly once when
// they are done with the stream — including the error case where
// Next() returns a non-nil err. Close cancels the subscription's
// context and drains the pump goroutine. Without it the pump can
// remain blocked on its outbound channel until the upstream
// subscription closes naturally (which, for a chat completion that
// is mid-token, may never happen if the caller's parent context
// stays alive).
type hugrStream struct {
	ch     chan streamItem
	sub    *types.Subscription
	cancel context.CancelFunc
	logger *slog.Logger
	model  string

	closeOnce sync.Once
}

func (s *hugrStream) Next(ctx context.Context) (model.Chunk, bool, error) {
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

// Close cancels the subscription's context. The underlying
// WebSocket reader returns immediately, the upstream provider sees
// the cancellation, and the pump goroutine drains and closes the
// out channel.
func (s *hugrStream) Close() error {
	s.closeOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		if s.sub != nil {
			s.sub.Cancel()
		}
		// Drain remaining items so the pump goroutine isn't blocked.
		go func() {
			for range s.ch {
			}
		}()
	})
	return nil
}

// readStreamEvent extracts a types.LLMStreamEvent from one row of an
// Arrow RecordBatch.
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
		case "thinking":
			ev.Thinking = stringVal(val)
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

// parseToolCalls parses the Hugr stream's ToolCalls string. The format
// varies by provider — see comment block in convert.go.
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
		var args any
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			return nil, fmt.Errorf("unmarshal tool_call args: %w (raw: %s)", err, truncated)
		}
		return []types.LLMToolCall{{Arguments: args}}, nil
	default:
		return nil, fmt.Errorf("unexpected tool_calls format (raw: %s)", truncated)
	}
}
