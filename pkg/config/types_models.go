package config

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
type ModelsConfig struct {
	Model          string                  `mapstructure:"model"`
	MaxTokens      int                     `mapstructure:"max_tokens"`
	Temperature    float32                 `mapstructure:"temperature"`
	ContextWindows map[string]int          `mapstructure:"context_windows"`
	DefaultBudget  int                     `mapstructure:"default_budget"`
	Mode           string                  `mapstructure:"mode"`
	Routes         map[string]ModelsConfig `mapstructure:"routes"`
}

const (
	ModeLocal  = "local"
	ModeRemote = "remote"
)
