package recap

import (
	"context"
	"log/slog"
	"time"

	"github.com/hugr-lab/query-engine/types"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// Deps carries the agent-level collaborators the recap extension needs.
// Router resolves the cheap summarizer model; Querier runs the
// server-side embedding_distance (nil disables change_confidence — the
// topic is still produced). Both may be nil in boot-test fixtures, in
// which case the extension degrades gracefully.
type Deps struct {
	Router  *model.ModelRouter
	Querier types.Querier
	AgentID string
	Logger  *slog.Logger
}

// Config carries operator-tunable knobs. Zero values resolve to the
// defaults in [NewExtension]. Sizes are configured in TOKENS (the
// runtime's char/4 heuristic converts internally) because the right
// values track the embedding model's input budget.
type Config struct {
	// MaxRecapTokens is the embedder-input budget for the effective
	// topic (compressed recap ⊕ tail). The tail folds into the
	// compressed recap before it crosses FoldThreshold of this. Default
	// 512 — tune to the embedding model.
	MaxRecapTokens int
	// FoldThreshold is the fraction of MaxRecapTokens at which the tail
	// is folded into the compressed recap. Default 0.75 — accumulate raw
	// below it (no model call), summarize above.
	FoldThreshold float64
	// MaxMessageTokens truncates each dialogue message before it enters
	// the tail, so one long turn can't dominate the topic. Default 512.
	MaxMessageTokens int
	// RecapTargetTokens is the size of the compressed LONG recap the fold
	// produces — and the model response cap. Decoupled from MaxRecapTokens
	// (the fold-trigger budget) so lowering the trigger for testing can't
	// strangle the response. Default 256 (a concise few-sentence recap).
	RecapTargetTokens int
	// Intent selects the model.Router route for the fold call. Default
	// [model.IntentSummarize] — explicitly cheap, fast, NON-reasoning.
	Intent model.Intent
	// BuildTimeout bounds a single fold (model call + distance). Default
	// 15s.
	BuildTimeout time.Duration
	// EmbedModel is the embedder name passed to embedding_distance.
	// Default "_system_embedder".
	EmbedModel string
}

// Extension is the agent-level recap singleton: a [FrameObserver] that
// accumulates the root session's recent dialogue into a tail and folds
// it (async) into a compact topic only when it grows past the threshold;
// a [Recovery] hook that replays the last compressed recap on restart;
// and a [StateInitializer] for the per-root-session handle.
type Extension struct {
	deps Deps
	cfg  Config

	// Resolved char budgets (cfg tokens × charsPerToken).
	maxRecapChars     int
	foldThresholdChars int
	maxMsgChars       int
	windowCapChars    int
}

// NewExtension constructs the recap extension and fills config defaults.
func NewExtension(deps Deps, cfg Config) *Extension {
	if cfg.MaxRecapTokens <= 0 {
		cfg.MaxRecapTokens = 512
	}
	if cfg.FoldThreshold <= 0 || cfg.FoldThreshold >= 1 {
		cfg.FoldThreshold = 0.75
	}
	if cfg.MaxMessageTokens <= 0 {
		cfg.MaxMessageTokens = 512
	}
	if cfg.RecapTargetTokens <= 0 {
		cfg.RecapTargetTokens = 256
	}
	if cfg.Intent == "" {
		cfg.Intent = model.IntentSummarize
	}
	if cfg.BuildTimeout <= 0 {
		cfg.BuildTimeout = 15 * time.Second
	}
	if cfg.EmbedModel == "" {
		cfg.EmbedModel = "_system_embedder"
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	maxRecapChars := cfg.MaxRecapTokens * charsPerToken
	return &Extension{
		deps:               deps,
		cfg:                cfg,
		maxRecapChars:      maxRecapChars,
		foldThresholdChars: int(float64(maxRecapChars) * cfg.FoldThreshold),
		maxMsgChars:        cfg.MaxMessageTokens * charsPerToken,
		// Safety valve only — the tail normally folds well below this.
		windowCapChars: maxRecapChars * 8,
	}
}

// Compile-time interface assertions.
var (
	_ extension.Extension        = (*Extension)(nil)
	_ extension.StateInitializer = (*Extension)(nil)
	_ extension.FrameObserver    = (*Extension)(nil)
	_ extension.Recovery         = (*Extension)(nil)
)

func (e *Extension) Name() string            { return providerName }
func (e *Extension) Lifetime() tool.Lifetime { return tool.LifetimePerAgent }

// InitState allocates a [sessionRecap] handle for ROOT sessions only
// (Depth 0). Subagents carry the task/wave brief as their topic and do
// not recap, so they get no handle — [FromState] returns nil for them
// and every recap path short-circuits.
func (e *Extension) InitState(_ context.Context, state extension.SessionState) error {
	if state.Depth() != 0 {
		return nil
	}
	state.SetValue(StateKey, &sessionRecap{})
	return nil
}
