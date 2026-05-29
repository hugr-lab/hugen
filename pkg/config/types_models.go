package config

import "time"

// ModelsConfig holds LLM routing + per-model tuning read from the
// `models:` block in config.yaml. Owned by pkg/config; pkg/models
// consumes it through ModelsView.
//
// Routes is intent-name → per-route config; unmatched intents fall
// back to the default model (Model field).
//
// ContextWindows + DefaultBudget declare each model's input-context
// capacity in tokens. Lookup precedence at the consumer:
//  1. ContextWindows[<resolved model name>]
//  2. DefaultBudget (when > 0)
//  3. Hard-coded floor (128 000) with one-shot INFO log per intent
//
// RetryMaxAttempts + RetryInitialBackoff control transient-error
// retries the subscription pump performs before any chunk reaches
// the caller (rate-limit / 5xx / network blips). Defaults — when
// the field is left at its zero value — are
// DefaultRetryMaxAttempts (10) and DefaultRetryInitialBackoff
// (500ms). Backoff doubles per attempt up to a 30s ceiling.
type ModelsConfig struct {
	Model               string                  `mapstructure:"model"`
	MaxTokens           int                     `mapstructure:"max_tokens"`
	Temperature         float32                 `mapstructure:"temperature"`
	ContextWindows      map[string]int          `mapstructure:"context_windows"`
	DefaultBudget       int                     `mapstructure:"default_budget"`
	Mode                string                  `mapstructure:"mode"`
	RetryMaxAttempts    int                     `mapstructure:"retry_max_attempts"`
	RetryInitialBackoff time.Duration           `mapstructure:"retry_initial_backoff"`

	// FirstBatchDeadline caps how long the subscription pump waits
	// for the FIRST batch of tokens from the model server before
	// auto-cancelling and feeding the cancel back into the retry
	// loop as a transient error. Zero falls back to the package
	// default (DefaultFirstBatchDeadline = 5 minutes). Set to a
	// negative value to disable (no deadline; pre-Phase-5 behaviour
	// where a hung backend stalls the session forever).
	//
	// Configurable per-route — set inside `routes.<intent>:` to
	// tune individual routes (e.g. checker on a small / flaky model
	// may want a shorter deadline). The deadline tracks first batch
	// only; mid-stream silence is covered by InterBatchDeadline.
	FirstBatchDeadline time.Duration `mapstructure:"first_batch_deadline"`

	// InterBatchDeadline caps the gap BETWEEN batches once streaming
	// has begun — a backend that streams a few tokens then wedges
	// (the first-batch deadline no longer applies). Zero falls back
	// to DefaultInterBatchDeadline; negative disables. Configurable
	// per-route. A progressing-but-slow stream is never killed (every
	// batch restamps the watchdog); only true mid-stream silence is.
	InterBatchDeadline time.Duration `mapstructure:"inter_batch_deadline"`

	Routes map[string]ModelsConfig `mapstructure:"routes"`

	// TierIntents maps a session tier (root/mission/worker) to the
	// model-router intent the runtime applies as the spawned child's
	// default. Empty or missing entries fall back to the router's
	// default. Per-role overrides (SubAgentRole.Intent in skill
	// manifests) still win over the tier default. Phase 4.2.2 §11.
	TierIntents map[string]string `mapstructure:"tier_intents" yaml:"tier_intents,omitempty"`
}

const (
	ModeLocal  = "local"
	ModeRemote = "remote"

	// DefaultRetryMaxAttempts is the number of transient-error retries
	// applied per chat-completion subscription before the error
	// propagates to the session. Picked as a sane ceiling for
	// rate-limit storms — long enough to absorb a multi-minute
	// 429 burst, short enough that a truly broken upstream surfaces
	// in finite time.
	DefaultRetryMaxAttempts = 10

	// DefaultRetryInitialBackoff seeds the exponential schedule:
	// 500ms × 2^attempt, capped by RetryMaxBackoff. With 10 attempts
	// the total budget is roughly 500ms + 1s + 2s + 4s + 8s + 16s +
	// 30s × 4 = ~150s.
	DefaultRetryInitialBackoff = 500 * time.Millisecond

	// DefaultFirstBatchDeadline is the package-wide default for the
	// pre-first-batch hang detector. Local model servers (llama.cpp,
	// vLLM) occasionally accept a chat-completion subscribe and then
	// produce nothing — without this deadline the session waits
	// forever. Five minutes covers cold-start warmup on large
	// quantised models but surfaces a truly stuck backend in finite
	// time.
	DefaultFirstBatchDeadline = 5 * time.Minute

	// DefaultInterBatchDeadline is the package-wide default gap
	// allowed between batches once a stream is flowing. A model
	// actively generating emits batches every few seconds; two
	// minutes of total mid-stream silence means the backend wedged
	// (observed live: LM Studio stalling on an oversized prefill).
	// Generous enough to absorb a thinking-model pause, short enough
	// to beat the multi-minute HTTP timeout that otherwise hangs the
	// session.
	DefaultInterBatchDeadline = 2 * time.Minute
)
