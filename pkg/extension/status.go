package extension

import (
	"context"
	"encoding/json"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// Phase 5.1b live status capabilities. All three are optional —
// extensions implement only the ones they need; the runtime
// asserts each and wires what applies.
//
// The contract is: status is purely event-driven and adapter-side
// state. No polling primitives, no `Manager.Status(id)`, no
// recursive `SessionSnapshot` type in `pkg/session`. The dedicated
// `liveview` extension folds frames (own session via
// [FrameObserver], children via [ChildFrameObserver]) into an
// in-memory projection and emits one synthetic frame at meaningful
// state changes. Adapters subscribe to root's outbox, receive
// those frames, render. Idle sessions stay silent — no heartbeats.
//
// Isolation note: [ChildFrameObserver] hands a parent's extension
// the raw child frames (tool_call args, reasoning text, etc.) that
// the pump would otherwise drain. This is RUNTIME-side
// observability only — the model on the parent never sees these
// frames, since the visibility filter
// (`pkg/session/visibility.go::projectFrameToHistory`) stays
// default-deny. The hook lets an extension build a status
// projection without violating subagent isolation toward the model.

// FrameObserver is an async hook called from `Session.emit()`
// after a frame persists to the event log. The extension may fold
// the frame into its own state. Implementations MUST be
// non-blocking — typically a non-blocking channel send into a
// goroutine the extension owns. The runtime treats `OnFrameEmit`
// as fire-and-forget; any blocking would stall the emit hot path.
type FrameObserver interface {
	OnFrameEmit(ctx context.Context, state SessionState, frame protocol.Frame)
}

// ChildFrameObserver is an async hook called from a parent
// session's `projectChildFrame` BEFORE the implicit drain. It
// surfaces every child frame that the pump's explicit cases
// (AgentMessage{Final}, Error, InquiryRequest, SessionTerminated)
// did not consume — tool_call, tool_result, reasoning, raw
// agent_message chunks, system_marker, child ExtensionFrames.
//
// childSessionID names the direct child the frame came from. The
// extension state passed in is the PARENT's. Implementations MUST
// be non-blocking (same contract as [FrameObserver]).
type ChildFrameObserver interface {
	OnChildFrame(ctx context.Context, parent SessionState, childSessionID string, frame protocol.Frame)
}

// StatusReporter lets an extension publish its current state as
// an opaque JSON blob. The receiving adapter interprets the blob
// per extension name (the key under which liveview will store it
// in its emit payload).
//
// Returning nil or an empty slice means "no contribution" —
// liveview omits the entry. Implementations are read-only and
// MAY be called synchronously from any goroutine; the contract
// is the extension's own per-session mutex discipline.
type StatusReporter interface {
	ReportStatus(ctx context.Context, state SessionState) json.RawMessage
}
