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
	"github.com/hugr-lab/hugen/pkg/protocol"
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
	agentID string
	logger  *slog.Logger
}

// NewExtension constructs the extension. agentID is the owning
// agent's id — stamped as the author ID on every emitted frame, so
// the stuck_nudge SystemMessage passes protocol.Validate (an empty
// author ID is rejected, which silently dropped the nudge before).
// logger may be nil — defaults to [slog.Default].
func NewExtension(agentID string, logger *slog.Logger) *Extension {
	if logger == nil {
		logger = slog.Default()
	}
	return &Extension{agentID: agentID, logger: logger}
}

// author returns the participant stamped on emitted frames. Mirrors
// the other extensions (plan / whiteboard / skill): ID = agentID,
// Kind = agent. A bare {Kind: agent} (no ID) fails protocol.Validate.
func (e *Extension) author() protocol.ParticipantInfo {
	return protocol.ParticipantInfo{ID: e.agentID, Kind: protocol.ParticipantAgent, Name: "hugen"}
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
