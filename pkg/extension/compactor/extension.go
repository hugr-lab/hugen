package compactor

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/hugr-lab/hugen/pkg/config"
	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

// Extension implements the capability surface defined in
// phase-5.2-compactor-spec.md §3.2. Built once per agent at
// runtime boot; per-session state lives on [*CompactorState]
// handles allocated by InitState.
//
// γ wires the operator-config surface as a
// [config.CompactorView] rather than a flattened [Config]
// snapshot. Each compaction-fire re-reads the view via
// [Extension.resolveTierConfig] — so a future config service
// that supports hot reload (the view's [config.CompactorView.OnUpdate]
// hook) flows through naturally without the extension caching
// a stale snapshot.
//
// view may be nil for fixtures / tests that never wire an
// operator config; the resolver falls back to [DefaultConfig]
// in that case.
type Extension struct {
	logger *slog.Logger
	view   config.CompactorView
	// staticCfg is the fallback baseline when view is nil. Set by
	// [NewExtensionWithConfig] for fixtures / scenario harness
	// setups that don't wire a full operator-config service. nil
	// when view is non-nil — production callers go view-only.
	staticCfg *Config
	deps      Deps
}

// Config carries the operator-tunable knobs. γ runs the
// three-layer resolver [Extension.resolveTierConfig] to overlay
// per-tier + per-skill + per-role overrides on top of the values
// here. Callers building a Config literally should mirror the
// defaults from [DefaultConfig].
//
// All numeric / bool fields hold the FULLY-RESOLVED value used
// at compaction time — the resolver applies overrides into a
// fresh Config copy before each fire, never mutating the
// Extension-owned baseline.
// Strategy names the compactor's history-management mode.
// Three values land in η:
//
//   - StrategyOff — no pruning, no LLM. ProvideHistory returns
//     the full in-memory projection. Operator opt-out used for
//     debugging and for fixtures that need raw history.
//   - StrategyWindow — FIFO of the last `WindowSize` projected
//     entries. No LLM. Replaces today's `defaultHistoryWindow=50`
//     resume-only stop-gap.
//   - StrategySummarize — the α-ε pipeline: LLM-summariser →
//     digest → Block C → KeptVerbatim inline + post-cutoff tail.
//
// η.1 ships the type + parsing only; ProvideHistory always
// returns the full projection (effectively `off`) until η.2
// flips the read path.
type Strategy string

const (
	StrategyOff       Strategy = "off"
	StrategyWindow    Strategy = "window"
	StrategySummarize Strategy = "summarize"
)

// ValidStrategy reports whether s is one of the known strategies.
// Empty string is treated as "fall back to default" by callers,
// not as a valid explicit choice.
func ValidStrategy(s Strategy) bool {
	switch s {
	case StrategyOff, StrategyWindow, StrategySummarize:
		return true
	}
	return false
}

// effectiveStrategy maps an arbitrary Strategy value onto a known
// one, defaulting empty / unknown to [StrategySummarize]. Used by
// hot-path consumers (OnFrameEmit, shouldCompact) so a Config
// constructed without going through [DefaultConfig] still resolves
// to a sane behaviour.
func effectiveStrategy(s Strategy) Strategy {
	if ValidStrategy(s) {
		return s
	}
	return StrategySummarize
}

type Config struct {
	// Strategy selects the history-management mode (see [Strategy]).
	// Default is [StrategySummarize] (matches α-ε behaviour). η.1
	// ships the field but ProvideHistory always returns the full
	// projection until η.2 wires per-strategy pruning.
	Strategy Strategy

	// WindowSize is the entry cap for [StrategyWindow]. Ignored by
	// other strategies. Zero falls back to the η default (50 for
	// general use; the per-tier `worker` overlay raises this to 80
	// per spec §8).
	WindowSize int

	// Enabled is the kill-switch. When false the trigger
	// predicate short-circuits and no compaction ever fires.
	Enabled bool

	// MaxTurns is the turn-count limb threshold — compaction
	// fires once the per-session user-turn count exceeds this
	// value (and other gates pass). 0 disables the limb.
	MaxTurns int

	// MaxTokens is the abs estimated-prompt-token threshold —
	// compaction fires once the running token estimate exceeds
	// this value (and other gates pass). 0 disables the limb.
	// Until a MaxPromptTokens accessor lands on pkg/model the
	// absolute floor is what the budget limb checks; see
	// TokenBudgetRatio below.
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

	// KeptVerbatimMax caps the number of entries the
	// [DigestPayload.KeptVerbatim] slice carries across
	// iterations. When the slice grows past this count after
	// an append, the oldest entries are dropped (FIFO) — the
	// first user_message is always pinned at index 0 so the
	// model never loses the original task framing. 0 disables
	// the cap. Spec §3.5 / §5.5.
	KeptVerbatimMax int

	// MinTurnGap is the anti-thrash gate — at least this many
	// completed user-turns must elapse between successive
	// compactions on the same session.
	MinTurnGap int

	// LLMTimeout caps each summariser / collapse model call.
	LLMTimeout time.Duration

	// LLMIntent selects the model.Router intent for the
	// summariser + collapse calls. Default is
	// [model.IntentSummarize].
	LLMIntent model.Intent

	// TokenBudgetRatio is the fraction-of-MaxPromptTokens limb
	// the spec wires for γ. Parsed end-to-end from the YAML
	// schema + skill manifest overrides so future code can flip
	// the gate on without a config breakage; current code uses
	// [MaxTokens] (absolute floor) because there's no
	// MaxPromptTokens accessor on pkg/model today. The field
	// rides through every resolve layer untouched so the
	// switch lands behind one local change in shouldCompact.
	TokenBudgetRatio float64

	// UIMarkerEnabled toggles the adapter-side transcript marker
	// ("─── history compacted (iter N, M msgs) ───") drawn at the
	// digest cutoff boundary. Defaults to true; operators can
	// turn it off via `compactor.ui_marker.enabled: false`. The
	// flag is global (no per-tier override) in v1 — operators
	// want either marker-on or marker-off. Resolved value is
	// surfaced via [Extension.ReportStatus] so adapters can read
	// the cutoff + flag from one liveview payload. Phase 5.2 δ.
	UIMarkerEnabled bool

	// Tiers maps tier label (root | mission | worker) to the
	// per-tier overlay applied during resolveTierConfig. nil /
	// missing tiers inherit the top-level Config verbatim. The
	// Extension treats this field as read-only at construction
	// time — resolveTierConfig clones the relevant overlay into
	// a fresh Config copy before applying skill / role overrides.
	Tiers map[string]TierOverride
}

// TierOverride is the per-tier overlay shape used during config
// resolution. All fields are pointers so an absent key is
// distinct from "set to zero" — the resolver only overwrites the
// fields the operator explicitly set.
type TierOverride struct {
	Strategy             *Strategy
	WindowSize           *int
	Enabled              *bool
	MaxTurns             *int
	MaxTokens            *int
	PreservedRecentTurns *int
	DigestMaxTokens      *int
	KeptVerbatimMax      *int
	MinTurnGap           *int
	LLMTimeout           *time.Duration
	LLMIntent            *model.Intent
	TokenBudgetRatio     *float64
}

// Deps bundles the agent-level dependencies the β pipeline
// needs. Router resolves models for summariser + collapse;
// Store backs ListEvents (the source of truth for selecting
// the newly-compactable range); AgentID stamps emitted frames;
// SkillCatalog backs the γ per-skill / per-role resolver
// (resolveTierConfig).
//
// All fields are required for compaction to fire. Trigger
// short-circuits when Router or Store is nil — α-style boot
// (tests, fixtures with no model wired) stays correct.
// SkillCatalog is optional: nil falls back to top-level + tier
// overlays only (no skill / role overrides applied).
type Deps struct {
	Router       *model.ModelRouter
	Store        StoreReader
	AgentID      string
	SkillCatalog SkillCatalog
}

// SkillCatalog is the narrow lookup surface the per-tier
// resolver consumes. Declared at the consumer per constitution
// principle III so tests can stub exactly the slice of pkg/skill
// the resolver needs without dragging the whole SkillManager
// (and its DuckDB store) into compactor's test fixtures.
//
// LookupCompactor returns (mission, role, nil) where mission is
// the skill-level override (or nil if absent) and role is the
// per-role override for the named role inside that skill (or nil
// if either the role is missing or no override was declared).
// Either side may be nil independently; both nil means "no
// manifest overrides for this (skill, role) pair". An error is
// reserved for catastrophic catalog failures and is treated as
// "no overrides" by the resolver — compaction never blocks on
// catalog problems.
type SkillCatalog interface {
	LookupCompactor(ctx context.Context, skill, role string) (mission *OverrideSpec, roleOverride *OverrideSpec, err error)
}

// OverrideSpec is the wire-neutral shape every catalog adapter
// projects into. Mirrors [TierOverride] field-for-field so the
// resolver can apply both kinds of overlay via one helper. Kept
// here (not at pkg/skill) so pkg/extension/compactor stays
// independent of the skill manifest's exact YAML tags.
type OverrideSpec struct {
	Strategy             *string
	WindowSize           *int
	Enabled              *bool
	MaxTurns             *int
	MaxTokens            *int
	PreservedRecentTurns *int
	DigestMaxTokens      *int
	KeptVerbatimMax      *int
	MinTurnGap           *int
	LLMTimeoutMs         *int
	LLMIntent            *string
	TokenBudgetRatio     *float64
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
		Strategy:             StrategySummarize,
		WindowSize:           50,
		Enabled:              true,
		MaxTurns:             50,
		MaxTokens:            80_000,
		PreservedRecentTurns: 10,
		DigestMaxTokens:      4_000,
		KeptVerbatimMax:      40,
		MinTurnGap:           3,
		LLMTimeout:           30 * time.Second,
		LLMIntent:            model.IntentSummarize,
		UIMarkerEnabled:      true,
	}
}

// NewExtension constructs the compactor extension wired to a
// [config.CompactorView]. logger may be nil — defaults to
// slog.Default(). view may be nil — the resolver falls back to
// [DefaultConfig] (no operator overrides) in that case. Deps
// may carry nil Router / Store — the trigger predicate
// short-circuits so tests / fixtures that never wire a model
// continue to work.
//
// Production callers use this path. Fixtures / scenario tests
// that bypass the operator-config service use
// [NewExtensionWithConfig] instead.
func NewExtension(logger *slog.Logger, view config.CompactorView, deps Deps) *Extension {
	if logger == nil {
		logger = slog.Default()
	}
	return &Extension{logger: logger, view: view, deps: deps}
}

// NewExtensionWithConfig constructs the extension with a flat
// [Config] baseline — no operator-config view. Used by
// integration fixtures + unit tests that want to control
// resolved fields directly without standing up a
// [config.CompactorView]. The Config value is copied; subsequent
// caller-side mutations do not affect the extension's baseline.
func NewExtensionWithConfig(logger *slog.Logger, cfg Config, deps Deps) *Extension {
	if logger == nil {
		logger = slog.Default()
	}
	cfgCopy := cfg
	return &Extension{logger: logger, staticCfg: &cfgCopy, deps: deps}
}

// baseConfig returns the resolver's starting point — the
// pre-tier-overlay, pre-skill-override snapshot. Reads from the
// configured [config.CompactorView] when available so a future
// hot-reload of operator config propagates without re-creating
// the extension. Falls back to the static baseline set via
// [NewExtensionWithConfig], then to [DefaultConfig].
func (e *Extension) baseConfig() Config {
	if e.view != nil {
		return BuildConfig(e.view.CompactorConfig(), e.logger)
	}
	if e.staticCfg != nil {
		return *e.staticCfg
	}
	return DefaultConfig()
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
	_ extension.StatusReporter   = (*Extension)(nil)
	_ extension.HistoryOwner     = (*Extension)(nil)
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

// StatusPayload is the wire shape emitted by [Extension.ReportStatus]
// and folded into the liveview/status frame under
// `extensions.compactor`. Adapters consume this to render the
// transcript marker at the cutoff boundary. Phase 5.2 δ — keep
// fields additive; older adapters tolerate unknown keys via
// encoding/json's default ignore-unknown behaviour.
type StatusPayload struct {
	// Iteration is the latest digest's iteration counter (≥ 1
	// when a digest is active).
	Iteration int `json:"iteration"`
	// CutoffSeq is the seq immediately AFTER which the live
	// (uncompacted) history begins. Adapters draw the marker
	// between the frame with seq == CutoffSeq and the next.
	CutoffSeq int64 `json:"cutoff_seq"`
	// BlocksCount is len(SummaryBlocks) — informational, drives
	// the marker label ("iter N, M msgs" — M here is
	// KeptCount, not BlocksCount; both fields surfaced so the
	// adapter can pick).
	BlocksCount int `json:"blocks_count"`
	// KeptCount is len(KeptVerbatim) — informational; the
	// adapter renders this as the "msgs" count on the marker.
	KeptCount int `json:"kept_count"`
	// BuiltAt is the time the latest digest was written.
	BuiltAt time.Time `json:"built_at"`
	// UIMarkerEnabled echoes the resolved config flag so the
	// adapter can read both the cutoff and the toggle from one
	// payload. Defaults to true; operator sets
	// `compactor.ui_marker.enabled: false` to suppress.
	UIMarkerEnabled bool `json:"ui_marker_enabled"`

	// AdvertiseTokens is the cached size of the last Block C
	// render in EstimateTokens units. Liveview folds this into
	// the per-extension breakdown of context_budget. Phase 5.2
	// (context-budget β).
	AdvertiseTokens int `json:"advertise_tokens,omitempty"`

	// HistoryTokens is the running size of the owned history
	// cache in EstimateTokens units — i.e. how many tokens the
	// model will receive on its next prompt build (Block C
	// excluded; that's AdvertiseTokens). Phase 5.2
	// (context-budget β).
	HistoryTokens int `json:"history_tokens,omitempty"`
}

// ReportStatus implements [extension.StatusReporter]. Returns the
// JSON encoding of the active digest's projection (iteration,
// cutoff_seq, blocks_count, kept_count, built_at) plus the
// resolved ui_marker_enabled flag. Returns nil when no digest has
// been written yet — liveview omits the entry from the payload in
// that case.
//
// The ui_marker_enabled flag rides the same projection so adapters
// can read both the cutoff + the toggle from one liveview frame,
// avoiding a separate lookup path through the runtime config
// surface. Reads the top-level [Extension.baseConfig] (not the
// per-session resolver) because the flag is a v1 global toggle
// per spec §11.7 — per-tier override is out-of-scope.
//
// Phase 5.2 δ.
func (e *Extension) ReportStatus(_ context.Context, state extension.SessionState) json.RawMessage {
	s := FromState(state)
	if s == nil {
		return nil
	}
	advertiseTokens := s.AdvertiseTokens()
	historyTokens := s.HistoryTokens()
	d := s.Digest()
	if d == nil {
		// Phase 5.2 β — pre-first-compaction sessions still
		// contribute history_tokens (the model's actual prompt
		// size). Emit a minimal payload so the liveview
		// aggregator wires them into context_budget.
		if historyTokens == 0 && advertiseTokens == 0 {
			return nil
		}
		payload := StatusPayload{
			AdvertiseTokens: advertiseTokens,
			HistoryTokens:   historyTokens,
			UIMarkerEnabled: e.baseConfig().UIMarkerEnabled,
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return nil
		}
		return data
	}
	payload := StatusPayload{
		Iteration:       d.Iteration,
		CutoffSeq:       d.CutoffSeq,
		BlocksCount:     len(d.SummaryBlocks),
		KeptCount:       len(d.KeptVerbatim),
		BuiltAt:         d.BuiltAt,
		UIMarkerEnabled: e.baseConfig().UIMarkerEnabled,
		AdvertiseTokens: advertiseTokens,
		HistoryTokens:   historyTokens,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		if e.logger != nil {
			e.logger.Warn("compactor: marshal status payload",
				"session", state.SessionID(), "err", err)
		}
		return nil
	}
	return data
}
