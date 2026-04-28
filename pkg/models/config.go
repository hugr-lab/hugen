package models

// Config holds LLM routing + default-model tuning read from `llm:` in
// config.yaml. Owned by pkg/models because the router + hugr-LLM
// adapter are the concrete consumers.
//
// Routes is intent-name → model-name; unmatched intents fall back to
// the default model (Model field).
//
// ContextWindows + DefaultBudget (spec 006) declare each model's input-
// context capacity in tokens. Consumers (notably pkg/chatcontext.Compactor)
// resolve the budget for an intent through Router.BudgetFor — the
// compactor stops assuming a hard-coded 128 000 budget and follows the
// actual model in use, so cheap local models (e.g. Qwen3-27B @ 100 000,
// Gemma-4 @ 50 000) compact at the right threshold instead of
// overflowing.
//
// Lookup precedence at Router.BudgetFor:
//  1. ContextWindows[<model name resolved from intent>]
//  2. DefaultBudget (when > 0)
//  3. Hard-coded floor of 128 000 (with a one-shot INFO log per intent)
//
// Both fields are optional: when omitted, the floor preserves today's
// behaviour. Set ContextWindows to model the cheap-model deployment and
// DefaultBudget to bias unknown models toward a safer cap.
type Config struct {
	Model          string            `mapstructure:"model"`
	MaxTokens      int               `mapstructure:"max_tokens"`
	Temperature    float32           `mapstructure:"temperature"`
	ContextWindows map[string]int    `mapstructure:"context_windows"`
	DefaultBudget  int               `mapstructure:"default_budget"`
	Mode           string            `mapstructure:"mode"`
	Routes         map[string]Config `mapstructure:"routes"`
}

const (
	LocalMode  = "local"
	RemoteMode = "remote"
)

// Shared model options: attach MaxTokens / Temperature from cfg in
// addition to the caller-supplied ones.
func (cfg Config) BuildOpts() []Option {
	var out []Option
	if cfg.MaxTokens > 0 {
		out = append(out, WithMaxTokens(cfg.MaxTokens))
	}
	if cfg.Temperature > 0 {
		out = append(out, WithTemperature(cfg.Temperature))
	}
	return out
}
