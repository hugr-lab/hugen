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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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

// ErrFirstBatchDeadline is returned by the subscription pump when
// the configured first-batch deadline elapses without any batch
// from the backend. It is treated as retryable by the runWithRetry
// loop — a hung llama.cpp / vLLM / network half-close that
// accepted the subscribe but never produced a token gets a fresh
// attempt instead of stalling the session forever.
var ErrFirstBatchDeadline = errors.New("hugrmodel: first batch deadline exceeded")

// ErrInterBatchDeadline is returned by the subscription pump when,
// AFTER the first batch landed, no further batch arrives within the
// configured inter-batch deadline — the backend committed to a stream
// then went silent mid-way (wedged llama.cpp / vLLM on a huge prefill,
// half-closed socket without a finish event). Retryable like
// ErrFirstBatchDeadline, but runWithRetry only retries when nothing
// was committed yet; a post-commit mid-stream stall surfaces the error
// to the session instead of hanging until the HTTP timeout.
var ErrInterBatchDeadline = errors.New("hugrmodel: inter-batch deadline exceeded")

// HugrModel implements pkg/model.Model against Hugr's GraphQL
// chat_completion subscription. Each Generate opens its own
// subscription; Stream.Close cancels the subscription's context,
// which propagates through the WebSocket so the upstream provider
// observes the cancellation within one network round-trip.
type HugrModel struct {
	name               string
	hugrModel          string
	querier            types.Querier
	logger             *slog.Logger
	maxTokens          int
	temperature        *float32
	toolChoiceFunc     func() string
	retry              retryPolicy
	firstBatchDeadline time.Duration
	interBatchDeadline time.Duration
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

// WithRetry configures transient-error retries on the chat
// completion subscription. maxAttempts caps the number of retries
// (0 = no retries); initialBackoff seeds an exponential schedule
// (initial × 2^attempt, capped at retryMaxBackoff = 30s). Retries
// only fire BEFORE the first chunk reaches the caller — once the
// model emits content / tool_use the stream is committed and the
// error propagates to session as today.
func WithRetry(maxAttempts int, initialBackoff time.Duration) Option {
	return func(m *HugrModel) { m.retry = newRetryPolicy(maxAttempts, initialBackoff) }
}

// WithFirstBatchDeadline caps how long the subscription pump waits
// for the FIRST batch of tokens before cancelling the subscription
// and feeding the cancel back into the retry loop as a transient
// error. Zero leaves the deadline disabled — the original
// stall-forever behaviour. A negative value also disables.
//
// Mid-stream stalls (first batch landed, then silence) are covered
// separately by WithInterBatchDeadline.
func WithFirstBatchDeadline(d time.Duration) Option {
	return func(m *HugrModel) {
		if d > 0 {
			m.firstBatchDeadline = d
		}
	}
}

// WithInterBatchDeadline caps the gap BETWEEN batches once streaming
// has begun. If no batch arrives within d after the previous one, the
// pump cancels the subscription and surfaces ErrInterBatchDeadline.
// Guards against a backend that streams a few tokens then wedges (the
// first-batch deadline no longer applies past the first batch). Zero
// or negative leaves it disabled.
func WithInterBatchDeadline(d time.Duration) Option {
	return func(m *HugrModel) {
		if d > 0 {
			m.interBatchDeadline = d
		}
	}
}

// NewHugr builds a HugrModel pinned to a specific Hugr LLM data
// source name (e.g. "gemma4-26b").
func NewHugr(q types.Querier, hugrModel string, opts ...Option) *HugrModel {
	m := &HugrModel{
		name:      "hugr-model",
		hugrModel: hugrModel,
		querier:   q,
		logger:    slog.Default(),
		retry:     newRetryPolicy(0, 0),
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
//
// Transient-error retry policy (configured via WithRetry, default
// no-retry) wraps Subscribe + the pre-first-chunk window: a 429 /
// 5xx / network blip that arrives before any chunk reaches the
// caller is retried with exponential backoff (initial × 2^attempt,
// capped 30s). Once the first content_delta / tool_use lands the
// stream is committed — subsequent errors propagate as today,
// because the caller has already begun assembling the assistant
// turn and a silent re-roll would corrupt the transcript.
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
	out := make(chan streamItem, 8)
	go m.runWithRetry(subCtx, vars, out)
	return &hugrStream{
		ch:     out,
		cancel: cancel,
		logger: m.logger,
		model:  m.hugrModel,
	}, nil
}

// runWithRetry is the top-level pump for a single Generate call.
// Drives Subscribe + pumpSubscription up to retry.maxAttempts+1
// times whenever a transient error fires before any chunk has
// reached the caller. Once committed (chunk sent on out), the
// retry loop exits and the error propagates as a streamItem.err.
//
// Closing out is this function's responsibility (matches the
// pre-retry contract of pumpSubscription) — caller blocks on the
// channel and stops when it closes.
func (m *HugrModel) runWithRetry(ctx context.Context, vars map[string]any, out chan<- streamItem) {
	defer close(out)

	var lastErr error
	maxAttempts := m.retry.maxAttempts
	for attempt := 0; attempt <= maxAttempts; attempt++ {
		if attempt > 0 {
			backoff := m.retry.nextBackoff(attempt)
			m.logger.Warn("hugr chat_completion retry",
				"model", m.hugrModel,
				"attempt", attempt,
				"max", maxAttempts,
				"backoff", backoff,
				"last_err", lastErr,
			)
			if err := m.retry.sleepBackoff(ctx, attempt); err != nil {
				_ = sendItem(ctx, out, streamItem{err: err})
				return
			}
		}

		subscribeStart := time.Now()
		sub, err := m.querier.Subscribe(ctx, chatCompletionSubscription, vars)
		if err != nil {
			lastErr = err
			m.logger.Error("hugr chat_completion subscribe failed",
				"model", m.hugrModel, "err", err, "attempt", attempt)
			if attempt < maxAttempts && isRetryableSubscribeErr(err) {
				continue
			}
			_ = sendItem(ctx, out, streamItem{err: fmt.Errorf("hugrmodel: subscribe: %w", err)})
			return
		}
		m.logger.Debug("hugr chat_completion subscribe ready",
			"model", m.hugrModel,
			"attempt", attempt,
			"elapsed_ms", time.Since(subscribeStart).Milliseconds(),
		)

		committed, err := m.pumpSubscription(ctx, sub, out, subscribeStart)
		if err == nil {
			return
		}
		lastErr = err
		if committed {
			// Stream already emitted a chunk to the caller — silent
			// retry would duplicate / re-order tokens. Surface the
			// error and exit; session decides next move.
			m.logger.Error("hugr chat_completion subscription failed (mid-stream, no retry)",
				"model", m.hugrModel, "err", err, "attempt", attempt)
			_ = sendItem(ctx, out, streamItem{err: fmt.Errorf("hugrmodel: subscription: %w", err)})
			return
		}
		if attempt < maxAttempts && isRetryableSubscribeErr(err) {
			m.logger.Warn("hugr chat_completion subscription failed (will retry)",
				"model", m.hugrModel, "err", err, "attempt", attempt)
			continue
		}
		m.logger.Error("hugr chat_completion subscription failed",
			"model", m.hugrModel, "err", err, "attempt", attempt)
		_ = sendItem(ctx, out, streamItem{err: fmt.Errorf("hugrmodel: subscription: %w", err)})
		return
	}
}

// pumpSubscription reads RecordBatches from the Hugr subscription,
// converts each row to a model.Chunk, and pushes onto out. Returns
// (committed, err): committed=true once any chunk has been delivered
// to the caller (retries forbidden past this point); err is non-nil
// on the failure path AND on canceled (caller distinguishes via
// isCanceled). Does NOT close out — runWithRetry owns the lifetime
// since it may re-enter this call on retry.
//
// The subscription's underlying websocket is released before this
// function returns regardless of outcome (sub.Cancel is idempotent
// per types.Subscription contract). Without the explicit cancel,
// retry would leak one ws-connection-equivalent per failed attempt
// for the lifetime of the parent ctx (typically the whole Generate
// call); on a 10-retry rate-limit storm that is 10 stranded
// subscriptions held until the caller's stream closes.
func (m *HugrModel) pumpSubscription(ctx context.Context, sub *types.Subscription, out chan<- streamItem, subscribeStart time.Time) (bool, error) {
	defer sub.Cancel()
	const completionPath = ""
	var finishEv types.LLMStreamEvent
	var sawFinish bool
	var batchCount, rowCount int
	var firstBatchAt time.Time
	var committed bool

	// First-batch deadline. When >0, a time.AfterFunc cancels the
	// pump's ctx if no batch arrives within m.firstBatchDeadline —
	// the backend has accepted the subscription but produced
	// nothing (hung llama.cpp / vLLM / network half-close without
	// finish event). Without this the session would wait forever.
	//
	// Race-safety: timer callback and the first-batch handler race
	// on `firstBatchSeen`. We use CompareAndSwap so EXACTLY ONE
	// path wins:
	//   - Timer's CAS succeeds  → no batch arrived before deadline.
	//     Set deadlineHit, cancel ctx, surface ErrFirstBatchDeadline.
	//   - Handler's CAS succeeds → a batch arrived; timer bails
	//     out on its own CAS attempt.
	// Without CAS, the timer could read `false` then cancel ctx
	// while the handler had already started rendering — that path
	// dropped the inflight batch without retry.
	pumpCtx := ctx
	var cancelPump context.CancelFunc
	var firstBatchSeen atomic.Bool
	var deadlineHit atomic.Bool
	var interBatchHit atomic.Bool
	var lastBatchNano atomic.Int64
	if m.firstBatchDeadline > 0 || m.interBatchDeadline > 0 {
		pumpCtx, cancelPump = context.WithCancel(ctx)
		defer cancelPump()
	}
	if m.firstBatchDeadline > 0 {
		timer := time.AfterFunc(m.firstBatchDeadline, func() {
			// CAS claims the "first batch not seen" state. If the
			// handler already flipped the flag we lost the race —
			// bail; the handler is mid-rendering and will complete.
			if !firstBatchSeen.CompareAndSwap(false, true) {
				return
			}
			deadlineHit.Store(true)
			cancelPump()
		})
		defer timer.Stop()
	}
	if m.interBatchDeadline > 0 {
		// Watchdog for mid-stream silence: once the first batch lands,
		// cancel the pump if the gap to the next batch exceeds the
		// deadline. lastBatchNano is restamped by the handler on every
		// batch; the watcher polls at a fraction of the deadline.
		watchStop := make(chan struct{})
		defer close(watchStop)
		go m.interBatchWatch(pumpCtx, watchStop, &firstBatchSeen, &lastBatchNano, &interBatchHit, cancelPump)
	}

	// Watchdog: if no batch arrives within 30s, log a heartbeat
	// every 30s so a stuck upstream LLM is observable in real time
	// instead of silent until budget elapsed. Stopped on every exit.
	heartbeatStop := make(chan struct{})
	defer close(heartbeatStop)
	go m.subscriptionHeartbeat(heartbeatStop, &batchCount, &rowCount, subscribeStart, &firstBatchAt)

	err := ReadSubscription(pumpCtx, sub, map[string]BatchHandler{
		completionPath: func(ctx context.Context, batch arrow.RecordBatch) error {
			// Restamp the inter-batch watchdog: every batch (content or
			// not) counts as liveness, so a slow-but-progressing stream
			// is never killed — only true mid-stream silence is.
			lastBatchNano.Store(time.Now().UnixNano())
			// CAS races the deadline timer for the "first batch"
			// state. If we win the swap, the timer's own CAS will
			// fail when it fires — safe to proceed even if the timer
			// is sleeping right at the deadline edge.
			if firstBatchAt.IsZero() && firstBatchSeen.CompareAndSwap(false, true) {
				firstBatchAt = time.Now()
				m.logger.Debug("hugr chat_completion first batch",
					"model", m.hugrModel,
					"elapsed_ms", time.Since(subscribeStart).Milliseconds(),
				)
			}
			batchCount++
			schema := batch.Schema()
			for i := 0; i < int(batch.NumRows()); i++ {
				if err := ctx.Err(); err != nil {
					return err
				}
				rowCount++
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
				committed = true
			}
			return nil
		},
	})
	if deadlineHit.Load() {
		// AfterFunc cancelled pumpCtx because no first batch
		// arrived inside the deadline. Surface as a retryable
		// error so runWithRetry can re-issue the subscription
		// instead of treating it as drained-success.
		return committed, fmt.Errorf("%w (no batch in %s)", ErrFirstBatchDeadline, m.firstBatchDeadline)
	}
	if interBatchHit.Load() {
		// Watchdog cancelled pumpCtx because the stream went silent
		// mid-flight. Retryable, but runWithRetry only retries when
		// nothing was committed yet — a post-commit stall surfaces to
		// the session (it ends the turn with an error) rather than
		// hanging until the HTTP timeout.
		return committed, fmt.Errorf("%w (no batch for %s mid-stream)", ErrInterBatchDeadline, m.interBatchDeadline)
	}
	if err != nil && !isCanceled(err) {
		return committed, err
	}
	if !sawFinish {
		// Subscription closed without a finish event. Treat as
		// canceled / drained — no terminal chunk to emit, no err.
		return committed, nil
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
				ID:   c.Call.ID,
				Name: c.Call.Name,
				Args: c.Call.Arguments,
				Hash: c.Hash,
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
	// A successful finish counts as committed — caller has the
	// terminal chunk in hand. Returning nil err short-circuits the
	// retry loop in runWithRetry.
	return true, nil
}

// interBatchWatch cancels the pump when the stream goes silent
// mid-flight: after the first batch is seen, if the gap since the
// last batch exceeds m.interBatchDeadline it CAS-claims the hit flag
// and cancels pumpCtx (the handler's restamp of lastBatchNano keeps a
// progressing stream alive). Polls at a fraction of the deadline,
// clamped to [1s, 15s]. Exits on pumpCtx cancellation, on watchStop
// close (pump teardown), or after firing once.
func (m *HugrModel) interBatchWatch(
	ctx context.Context,
	stop <-chan struct{},
	firstSeen *atomic.Bool,
	lastNano *atomic.Int64,
	hit *atomic.Bool,
	cancel context.CancelFunc,
) {
	tick := m.interBatchDeadline / 4
	if tick < time.Second {
		tick = time.Second
	}
	if tick > 15*time.Second {
		tick = 15 * time.Second
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-t.C:
			if !firstSeen.Load() {
				continue // streaming hasn't begun; first-batch deadline owns this window
			}
			last := lastNano.Load()
			if last == 0 {
				continue
			}
			if time.Since(time.Unix(0, last)) >= m.interBatchDeadline {
				if hit.CompareAndSwap(false, true) {
					m.logger.Warn("hugr chat_completion inter-batch deadline",
						"model", m.hugrModel,
						"deadline", m.interBatchDeadline,
						"since_last_batch_ms", time.Since(time.Unix(0, last)).Milliseconds(),
					)
					cancel()
				}
				return
			}
		}
	}
}

// subscriptionHeartbeat logs a Debug message every 30s while the
// subscription is open with no batches received. Surfaces stuck
// upstream LLM calls so the operator sees the stall in real time
// instead of silent-until-budget-elapsed. Returns when stop is
// closed (always — pumpSubscription's defer guarantees it).
func (m *HugrModel) subscriptionHeartbeat(stop <-chan struct{}, batchCount, rowCount *int, subscribeStart time.Time, firstBatchAt *time.Time) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			elapsed := time.Since(subscribeStart)
			fields := []any{
				"model", m.hugrModel,
				"elapsed_s", int(elapsed.Seconds()),
				"batches", *batchCount,
				"rows", *rowCount,
			}
			if firstBatchAt.IsZero() {
				m.logger.Debug("hugr chat_completion still waiting for first batch", fields...)
			} else {
				fields = append(fields, "first_batch_after_ms", time.Since(*firstBatchAt).Milliseconds())
				m.logger.Debug("hugr chat_completion still streaming", fields...)
			}
		}
	}
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
			ID:   first.Call.ID,
			Name: first.Call.Name,
			Args: first.Call.Arguments,
			Hash: first.Hash,
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
// the cancellation, and runWithRetry drains and closes the out
// channel. Sub.Cancel is not called explicitly here — runWithRetry
// owns the *types.Subscription instance(s) (one per retry attempt)
// and they all share this ctx; canceling it propagates to all of
// them.
func (s *hugrStream) Close() error {
	s.closeOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
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

// parsedToolCall pairs a fully-decoded LLMToolCall (Arguments
// resolved to a Go value, ready for downstream JSON re-marshaling
// or direct dispatch) with a stable per-call hash computed from
// the raw bytes. Hashing here avoids re-marshaling Arguments back
// to JSON later — the raw form is already on the wire.
type parsedToolCall struct {
	Call types.LLMToolCall
	Hash string
}

// parseToolCalls parses the Hugr stream's ToolCalls string. The
// format varies by provider — see comment block in convert.go.
// The function uses a json.RawMessage shim for Arguments so the
// hash sees the bytes verbatim (whitespace + key order as the
// provider sent them — providers are deterministic enough that
// this gives a stable repeat-detection signal).
func parseToolCalls(raw string) ([]parsedToolCall, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	truncated := raw
	if len(truncated) > 200 {
		truncated = truncated[:200] + "..."
	}
	type rawCall struct {
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	var calls []rawCall
	switch {
	case strings.HasPrefix(raw, "["):
		if err := json.Unmarshal([]byte(raw), &calls); err != nil {
			return nil, fmt.Errorf("unmarshal tool_calls array: %w (raw: %s)", err, truncated)
		}
	case strings.HasPrefix(raw, "{"):
		var single rawCall
		if err := json.Unmarshal([]byte(raw), &single); err != nil {
			return nil, fmt.Errorf("unmarshal tool_call object: %w (raw: %s)", err, truncated)
		}
		if single.Name == "" {
			// Provider streamed only the args envelope — fold it
			// into a nameless call. Caller (LLM-side) must not
			// have asked tools, so this is best-effort.
			single.Arguments = json.RawMessage(raw)
		}
		calls = []rawCall{single}
	default:
		return nil, fmt.Errorf("unexpected tool_calls format (raw: %s)", truncated)
	}

	out := make([]parsedToolCall, 0, len(calls))
	for _, rc := range calls {
		var args any
		if len(rc.Arguments) > 0 {
			if err := json.Unmarshal(rc.Arguments, &args); err != nil {
				return nil, fmt.Errorf("unmarshal tool_call args for %q: %w (raw: %s)", rc.Name, err, truncated)
			}
		}
		out = append(out, parsedToolCall{
			Call: types.LLMToolCall{ID: rc.ID, Name: rc.Name, Arguments: args},
			Hash: hashToolCall(rc.Name, rc.Arguments),
		})
	}
	return out, nil
}

// hashToolCall returns a deterministic identifier for a single
// tool call. Computed from the raw arguments bytes (no canonical
// re-marshaling) so it costs one sha-256 pass and nothing else.
// Repeat detection compares this string — equal hash → same call.
func hashToolCall(name string, rawArgs []byte) string {
	h := sha256.New()
	h.Write([]byte(name))
	h.Write([]byte{0})
	h.Write(rawArgs)
	return hex.EncodeToString(h.Sum(nil))
}
