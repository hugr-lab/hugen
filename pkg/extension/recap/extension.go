package recap

import (
	"context"
	"log/slog"
	"time"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// Deps carries the agent-level collaborators the recap extension needs.
// Router resolves the cheap summarizer model; it may be nil in boot-test
// fixtures, in which case the fold degrades gracefully (no marker formed,
// the raw recent ring still backs CurrentRecap).
type Deps struct {
	Router  *model.ModelRouter
	AgentID string
	Logger  *slog.Logger
}

// Config carries operator-tunable knobs. Zero values resolve to the
// defaults in [NewExtension].
type Config struct {
	// MaxMessageTokens caps each dialogue message before it enters the
	// ring — a generous bound (not a tight cut) so a full subagent task
	// reaches the summariser intact and the fold distils it. Default 4096
	// (≈ a full delegated task with headroom); short chat messages are
	// unaffected. Lower it for a small-context local summariser.
	MaxMessageTokens int
	// RingMessages bounds how many recent messages the ring keeps. Default
	// 8 — enough for the new user message(s) plus a couple of prior pairs.
	RingMessages int
	// RecentContext bounds how many prior (already-answered) messages the
	// fold prompt shows as context, beyond the turn's new user messages.
	// Default 4 (≈ the last two exchanges).
	RecentContext int
	// RecapTargetTokens is the size of the marker the fold produces — and
	// the model response cap. Default 256 (topic + a concise theme).
	RecapTargetTokens int
	// Intent selects the model.Router route for the fold call. Default
	// [model.IntentSummarize] — explicitly cheap, fast, NON-reasoning.
	Intent model.Intent
	// BuildTimeout bounds a single fold (the synchronous model call).
	// Default 15s.
	BuildTimeout time.Duration
}

// Extension is the agent-level recap singleton: a [FrameObserver] that
// accumulates the root session's recent dialogue into a bounded ring, a
// [TurnBoundaryHook] that (re)forms the topic marker synchronously before
// the turn renders (every turn for root; once at start for a subagent), a
// [Recovery] hook that replays the last marker on restart, and a
// [StateInitializer] for the per-session handle.
type Extension struct {
	deps Deps
	cfg  Config

	maxMsgChars   int // per-message truncation (cfg tokens × charsPerToken)
	maxRing       int // ring size in messages
	recentContext int // prior messages shown as fold context
}

// NewExtension constructs the recap extension and fills config defaults.
func NewExtension(deps Deps, cfg Config) *Extension {
	if cfg.MaxMessageTokens <= 0 {
		cfg.MaxMessageTokens = 4096
	}
	if cfg.RingMessages <= 0 {
		cfg.RingMessages = 8
	}
	if cfg.RecentContext <= 0 {
		cfg.RecentContext = 4
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
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &Extension{
		deps:          deps,
		cfg:           cfg,
		maxMsgChars:   cfg.MaxMessageTokens * charsPerToken,
		maxRing:       cfg.RingMessages,
		recentContext: cfg.RecentContext,
	}
}

// Compile-time interface assertions.
var (
	_ extension.Extension        = (*Extension)(nil)
	_ extension.StateInitializer = (*Extension)(nil)
	_ extension.FrameObserver    = (*Extension)(nil)
	_ extension.TurnBoundaryHook = (*Extension)(nil)
	_ extension.Recovery         = (*Extension)(nil)
)

func (e *Extension) Name() string            { return providerName }
func (e *Extension) Lifetime() tool.Lifetime { return tool.LifetimePerAgent }

// InitState allocates a [sessionRecap] handle for EVERY session. Root
// (Depth 0) re-forms a rolling marker each turn from the conversation; a
// subagent forms its marker ONCE at start by distilling its task message
// (its goal is fixed). The `root` flag drives that cadence in
// [Extension.OnTurnBoundary].
func (e *Extension) InitState(_ context.Context, state extension.SessionState) error {
	state.SetValue(StateKey, &sessionRecap{root: state.Depth() == 0})
	return nil
}
