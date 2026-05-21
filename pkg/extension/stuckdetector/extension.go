// Package stuckdetector implements the four rising-edge stuck
// detectors from phase-4-spec §8.3 as a session extension.
// Phase 5.2.η.4 moved this out of `pkg/session/turn_stuck.go` so
// the session goroutine doesn't carry detector state; the
// extension observes [protocol.ToolResult] / consolidated
// [protocol.AgentMessage] frames via [extension.FrameObserver]
// and emits one [protocol.SystemMessage]{stuck_nudge} (or, for
// no_progress, one [protocol.SystemMarker]{no_progress}) per
// inactive→active transition.
//
// Soft warning + hard ceiling stay in `pkg/session/turn_ceiling.go`
// — those depend on `turnState.iter` / `turnState.cap` which
// are turn-loop internals; exposing them through SessionState is
// out of scope for η.4.
package stuckdetector

import (
	"context"
	"log/slog"

	"github.com/hugr-lab/hugen/pkg/extension"
)

// StateKey is the [extension.SessionState] key the detector
// stores its per-session [*DetectorState] handle under.
const StateKey = "stuckdetector"

// providerName doubles as the extension's [Extension.Name] and
// shows up in observability traces.
const providerName = "stuckdetector"

// Extension is the package's [extension.Extension] handle. One
// per agent runtime; per-session state lives in DetectorState
// attached to [extension.SessionState] via InitState.
type Extension struct {
	logger *slog.Logger
}

// NewExtension constructs the extension. logger may be nil —
// defaults to [slog.Default].
func NewExtension(logger *slog.Logger) *Extension {
	if logger == nil {
		logger = slog.Default()
	}
	return &Extension{logger: logger}
}

// Name implements [extension.Extension].
func (e *Extension) Name() string { return providerName }

// InitState implements [extension.StateInitializer]. Allocates a
// fresh [*DetectorState] handle for the calling session.
func (e *Extension) InitState(_ context.Context, state extension.SessionState) error {
	state.SetValue(StateKey, &DetectorState{})
	return nil
}

// FromState resolves the [*DetectorState] handle attached to
// state, or nil when the extension wasn't wired (fixture
// sessions skipping the runtime). Callers gate on nil.
func FromState(state extension.SessionState) *DetectorState {
	if state == nil {
		return nil
	}
	v, ok := state.Value(StateKey)
	if !ok {
		return nil
	}
	s, _ := v.(*DetectorState)
	return s
}

// Compile-time interface assertions.
var (
	_ extension.Extension        = (*Extension)(nil)
	_ extension.StateInitializer = (*Extension)(nil)
	_ extension.FrameObserver    = (*Extension)(nil)
)
