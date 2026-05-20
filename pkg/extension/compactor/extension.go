package compactor

import (
	"context"
	"log/slog"
	"time"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

// Extension implements the capability surface defined in
// phase-5.2-compactor-spec.md §3.2. Built once per agent at
// runtime boot; per-session state lives on [*CompactorState]
// handles allocated by InitState.
//
// Phase β fills in the LLM summariser pipeline + per-Kind
// dispatch + cap-driven collapse. γ will add per-tier config /
// per-skill manifest overrides; until then a single flat Config
// governs every session.
type Extension struct {
	logger *slog.Logger
	cfg    Config
	deps   Deps
}

// Config carries the operator-tunable knobs. Phase β is flat —
// γ rewrites this into a per-tier resolver. Field defaults are
// applied by [DefaultConfig]; callers building a Config
// literally should mirror those.
type Config struct {
	// Enabled is the global kill-switch. When false the trigger
	// predicate short-circuits and no compaction ever fires.
	Enabled bool

	// MaxTurns is the turn-count limb threshold — compaction
	// fires once the per-session user-turn count exceeds this
	// value (and other gates pass). 0 disables the limb.
	MaxTurns int

	// MaxTokens is the abs estimated-prompt-token threshold —
	// compaction fires once the running token estimate exceeds
	// this value (and other gates pass). 0 disables the limb.
	// γ will replace this with a ratio against the resolved
	// model's MaxPromptTokens; β uses an absolute floor.
	MaxTokens int

	// PreservedRecentTurns is the minimum number of completed
	// user-turns the compactor keeps verbatim past CutoffSeq —
	// the live tail the model sees unmodified. See spec §3.5.
	PreservedRecentTurns int

	// DigestMaxTokens is the cap that triggers cap-driven
	// collapse (spec §5.5). When estimateDigestTokens exceeds
	// this, every SummaryBlock is rolled up into a single one
	// via a second LLM call.
	DigestMaxTokens int

	// MinTurnGap is the anti-thrash gate — at least this many
	// completed user-turns must elapse between successive
	// compactions on the same session.
	MinTurnGap int

	// LLMTimeout caps each summariser / collapse model call.
	LLMTimeout time.Duration

	// LLMIntent selects the model.Router intent for the
	// summariser + collapse calls. Default is
	// [model.IntentSummarize]; γ may introduce a dedicated
	// "compact" intent.
	LLMIntent model.Intent
}

// Deps bundles the agent-level dependencies the β pipeline
// needs. Router resolves models for summariser + collapse;
// Store backs ListEvents (the source of truth for selecting
// the newly-compactable range); AgentID stamps emitted frames.
//
// All fields are required for compaction to fire. Trigger
// short-circuits when Router or Store is nil — α-style boot
// (tests, fixtures with no model wired) stays correct.
type Deps struct {
	Router  *model.ModelRouter
	Store   StoreReader
	AgentID string
}

// StoreReader is the narrow slice of [store.RuntimeStore] the
// β pipeline consumes. Declared at the consumer per
// constitution principle III so tests can fake exactly this
// surface without dragging in DuckDB.
type StoreReader interface {
	ListEvents(ctx context.Context, sessionID string, opts store.ListEventsOpts) ([]store.EventRow, error)
}

// DefaultConfig returns the β defaults applied when the
// operator's agent_config.yaml carries no `compactor:` block.
// γ replaces this with a per-tier resolver.
func DefaultConfig() Config {
	return Config{
		Enabled:              true,
		MaxTurns:             50,
		MaxTokens:            80_000,
		PreservedRecentTurns: 10,
		DigestMaxTokens:      4_000,
		MinTurnGap:           3,
		LLMTimeout:           30 * time.Second,
		LLMIntent:            model.IntentSummarize,
	}
}

// NewExtension constructs the compactor extension. logger may
// be nil — defaults to slog.Default(). Deps may carry nil
// Router / Store — the trigger predicate short-circuits in
// that case so tests / fixtures that never wire a model
// continue to work.
func NewExtension(logger *slog.Logger, cfg Config, deps Deps) *Extension {
	if logger == nil {
		logger = slog.Default()
	}
	return &Extension{logger: logger, cfg: cfg, deps: deps}
}

// Compile-time interface assertions — every capability the
// extension claims to satisfy gets a compile-time check so a
// future signature change surfaces here rather than at runtime.
var (
	_ extension.Extension        = (*Extension)(nil)
	_ extension.StateInitializer = (*Extension)(nil)
	_ extension.Recovery         = (*Extension)(nil)
	_ extension.Advertiser       = (*Extension)(nil)
	_ extension.FrameObserver    = (*Extension)(nil)
	_ extension.TurnBoundaryHook = (*Extension)(nil)
)

// Name implements [extension.Extension].
func (e *Extension) Name() string { return providerName }

// InitState implements [extension.StateInitializer]. Allocates a
// fresh [*CompactorState] handle for the calling session.
func (e *Extension) InitState(_ context.Context, state extension.SessionState) error {
	state.SetValue(StateKey, &CompactorState{})
	return nil
}

// FromState resolves the [*CompactorState] handle attached to
// state, or nil if the extension's StateInitializer never ran
// (a misconfigured runtime that omitted the extension from
// phase-8 wiring). Callers gate on nil before reading.
func FromState(state extension.SessionState) *CompactorState {
	if state == nil {
		return nil
	}
	v, ok := state.Value(StateKey)
	if !ok {
		return nil
	}
	s, _ := v.(*CompactorState)
	return s
}
