package compactor

import (
	"context"
	"log/slog"
	"time"

	"github.com/hugr-lab/hugen/pkg/extension"
)

// Extension implements the capability surface defined in
// phase-5.2-compactor-spec.md §3.2. Built once per agent at
// runtime boot; per-session state lives on [*CompactorState]
// handles allocated by InitState.
//
// Phase α (this commit): wired but inert — the LLM call is
// stubbed and the trigger predicate returns false unconditionally
// pending the β milestone landing the dispatch + summariser
// pipeline. Sibling capabilities (Recovery, Advertiser,
// FrameObserver) are real so a future restart that sees
// persisted digest_set frames from a forward-rolled binary
// replays correctly.
type Extension struct {
	logger *slog.Logger
	cfg    Config
}

// Config carries the operator-tunable knobs. Phase α exposes
// only a subset; the full per-tier strategy lands in γ.
type Config struct {
	// Enabled is the global kill-switch. When false, the
	// trigger predicate short-circuits and no compaction ever
	// fires. Default true via runtime.Build.
	Enabled bool

	// LLMTimeout caps the per-compaction model call. Phase α
	// reads it but doesn't call the model; β puts it to use.
	LLMTimeout time.Duration
}

// DefaultConfig returns the conservative defaults the runtime
// applies when the operator's agent_config.yaml carries no
// `compactor:` block. Phase α — full per-tier defaults land in
// γ alongside the YAML schema.
func DefaultConfig() Config {
	return Config{
		Enabled:    true,
		LLMTimeout: 30 * time.Second,
	}
}

// NewExtension constructs the compactor extension. logger may be
// nil — defaults to slog.Default(); cfg falls back to
// [DefaultConfig] when its Enabled flag has never been set
// explicitly (zero-value detection via a separate sentinel is
// reserved for γ).
func NewExtension(logger *slog.Logger, cfg Config) *Extension {
	if logger == nil {
		logger = slog.Default()
	}
	return &Extension{logger: logger, cfg: cfg}
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
