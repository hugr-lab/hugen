// Package liveview is the phase-5.1b session-status aggregator
// and publisher. Each session gets a per-session liveview handle
// that:
//
//   - observes its OWN persisted frames via [FrameObserver]
//     (tool_call, reasoning, agent_message, inquiry_*,
//     subagent_*, session_status, …);
//   - observes its direct CHILD sessions' frames via
//     [ChildFrameObserver] (the pump hook fires for every child
//     frame that the explicit projection cases didn't consume,
//     before the implicit drain — see
//     pkg/session/subagent_pump.go);
//   - folds the observed events into an in-memory activity
//     projection (own snapshot + per-child cached snapshot);
//   - publishes one `ExtensionFrame{liveview/status,
//     Category:Marker}` carrying the projection on lifecycle
//     changes or after a 2-second debounce window. Frames go out
//     via [SessionState.OutboxOnly] — transient observability,
//     never written to the event log.
//
// Each level's liveview holds the projection of its own subtree:
// liveview-on-root → knows about root + mission (via the
// child-frame hook) + mission's own children projection
// (delivered as bubbled child frames). Adapters subscribe to
// root.Outbox() and decode the nested children map from each
// status frame.
//
// Push policy (event-driven, no heartbeats):
//
//   - Debounce 2 seconds between consecutive emits.
//   - Force-emit (skip debounce) on lifecycle-changing events:
//     turn boundary (idle ↔ active), pending-inquiry set / cleared,
//     subagent started / finished.
//   - No emit on idle. Silent sessions stay silent; the adapter
//     renders the last known state until the next emit.
//
// Capability surface implemented:
//
//   - [extension.Extension] (Name / Lifetime)
//   - [extension.StateInitializer] (per-session goroutine + state)
//   - [extension.FrameObserver]
//   - [extension.ChildFrameObserver]
//   - [extension.StatusReporter] (called by siblings' liveview
//     when they assemble their emit payload; liveview reports its
//     own activity projection)
//   - [extension.Closer] (drains the channel, exits the goroutine)
package liveview

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// providerName doubles as Extension.Name() and the ExtensionFrame
// Extension key adapters interpret. Stable per ABI contract.
const providerName = "liveview"

// opStatus is the ExtensionFrame.Op the liveview status emit
// uses. Adapters dispatch on the (Extension, Op) pair.
const opStatus = "status"

// defaultDebounce is the minimum interval between two
// consecutive non-forced emits. Phase 5.1b §"Push policy".
const defaultDebounce = 2 * time.Second

// channelBuffer caps the per-session observer goroutine inbox.
// Frame dispatch from emit / pump uses non-blocking send;
// overflow drops the frame with a warn-log. 256 mirrors the
// session outbox sizing.
const channelBuffer = 256

// Extension is the agent-level liveview singleton. State lives
// per-session in [*sessionView]; the singleton itself only
// implements the capability methods that take a SessionState.
type Extension struct {
	logger *slog.Logger
}

// New constructs the liveview extension. logger is optional;
// nil falls back to slog.Default.
func New(logger *slog.Logger) *Extension {
	if logger == nil {
		logger = slog.Default()
	}
	return &Extension{logger: logger}
}

// Compile-time interface assertions.
var (
	_ extension.Extension          = (*Extension)(nil)
	_ extension.StateInitializer   = (*Extension)(nil)
	_ extension.FrameObserver      = (*Extension)(nil)
	_ extension.ChildFrameObserver = (*Extension)(nil)
	_ extension.StatusReporter     = (*Extension)(nil)
	_ extension.Closer             = (*Extension)(nil)
)

// stateKey is the key under which the per-session handle lives.
const stateKey = providerName

// Name implements [extension.Extension].
func (e *Extension) Name() string { return providerName }

// Lifetime implements [tool.ToolProvider]. liveview contributes
// no tools to the catalogue; the constant is kept for the
// runtime's lifetime classifier consistency.
func (e *Extension) Lifetime() tool.Lifetime { return tool.LifetimePerAgent }

// InitState implements [extension.StateInitializer]. Allocates
// a fresh per-session view, spawns its observer goroutine, and
// stashes the handle under [stateKey]. The session-state handle
// is captured on the view so the goroutine can call OutboxOnly
// / Extensions when it emits.
func (e *Extension) InitState(ctx context.Context, state extension.SessionState) error {
	v := &sessionView{
		sessionID: state.SessionID(),
		depth:     state.Depth(),
		state:     state,
		logger:    e.logger,
		debounce:  defaultDebounce,
		ch:        make(chan frameEvent, channelBuffer),
		children:  map[string]json.RawMessage{},
	}
	v.wg.Add(1)
	go v.run()
	state.SetValue(stateKey, v)
	return nil
}

// OnFrameEmit implements [extension.FrameObserver]. Non-blocking
// dispatch of the OWN session frame into the observer goroutine.
// Overflow drops with a warn-log.
func (e *Extension) OnFrameEmit(ctx context.Context, state extension.SessionState, f protocol.Frame) {
	v := fromState(state)
	if v == nil {
		return
	}
	v.push(frameEvent{kind: ownFrame, frame: f})
}

// OnChildFrame implements [extension.ChildFrameObserver].
// Non-blocking dispatch of a CHILD frame tagged with childID.
func (e *Extension) OnChildFrame(ctx context.Context, parent extension.SessionState, childID string, f protocol.Frame) {
	v := fromState(parent)
	if v == nil {
		return
	}
	v.push(frameEvent{kind: childFrame, childID: childID, frame: f})
}

// ReportStatus implements [extension.StatusReporter]. Returns
// the JSON encoding of the session's current activity projection
// (NOT including the children map — that's liveview's own emit
// payload, not the report; siblings calling ReportStatus see
// only the local activity).
func (e *Extension) ReportStatus(_ context.Context, state extension.SessionState) json.RawMessage {
	v := fromState(state)
	if v == nil {
		return nil
	}
	return v.reportActivityJSON()
}

// CloseSession implements [extension.Closer]. Drains the channel
// and waits for the observer goroutine to exit so a session
// teardown completes deterministically.
func (e *Extension) CloseSession(_ context.Context, state extension.SessionState) error {
	v := fromState(state)
	if v == nil {
		return nil
	}
	v.shutdown()
	return nil
}

// fromState recovers the per-session handle, returning nil when
// InitState did not run (e.g. tests that mount a SessionState
// without registering the extension).
func fromState(state extension.SessionState) *sessionView {
	v, ok := state.Value(stateKey)
	if !ok || v == nil {
		return nil
	}
	h, _ := v.(*sessionView)
	return h
}

// ---------- per-session view ----------

// frameEventKind discriminates the two ingress paths into the
// observer goroutine.
type frameEventKind int

const (
	ownFrame   frameEventKind = iota // observed via FrameObserver
	childFrame                       // observed via ChildFrameObserver
)

// frameEvent is the message the observer goroutine consumes.
type frameEvent struct {
	kind    frameEventKind
	childID string // populated when kind == childFrame
	frame   protocol.Frame
}

// sessionView holds the per-session state. Owned exclusively by
// the observer goroutine after construction; the only external
// touch points are the channel send (non-blocking) and the
// shutdown signal.
type sessionView struct {
	sessionID string
	depth     int
	state     extension.SessionState // captured at InitState
	logger    *slog.Logger
	debounce  time.Duration

	ch        chan frameEvent
	closeOnce sync.Once
	wg        sync.WaitGroup

	// Activity state — owned by the observer goroutine, copied
	// out under reportMu when ReportStatus is called from another
	// goroutine (a sibling's liveview at emit-assembly time).
	reportMu       sync.Mutex
	lifecycleState string
	lastTool       *protocol.ToolCallRef
	pendingInquiry *protocol.PendingInquiryRef
	turnsUsed      int

	// children stores the LAST KNOWN status JSON each direct
	// child published via its own liveview status frame. Keyed
	// by child sessionID.
	children map[string]json.RawMessage
}

// push is the non-blocking enqueue called from FrameObserver /
// ChildFrameObserver. Overflow drops with a warn so the emit
// hot path stays unaffected.
func (v *sessionView) push(ev frameEvent) {
	select {
	case v.ch <- ev:
	default:
		if v.logger != nil {
			v.logger.Warn("liveview: observer channel overflow; dropping frame",
				"session", v.sessionID, "kind", ev.kind)
		}
	}
}

// shutdown signals the observer goroutine to exit and waits for
// it. Safe to call multiple times.
func (v *sessionView) shutdown() {
	v.closeOnce.Do(func() {
		close(v.ch)
	})
	v.wg.Wait()
}

// reportActivityJSON returns the JSON encoding of this session's
// own activity projection. Used by siblings' liveview at
// emit-assembly time (their ReportStatus iterates Extensions()
// and calls each StatusReporter; ours returns this blob). The
// children map is NOT included — that's the emit payload, not
// the report.
func (v *sessionView) reportActivityJSON() json.RawMessage {
	v.reportMu.Lock()
	defer v.reportMu.Unlock()
	body := map[string]any{}
	if v.lifecycleState != "" {
		body["lifecycle_state"] = v.lifecycleState
	}
	if v.lastTool != nil {
		body["last_tool_call"] = v.lastTool
	}
	if v.pendingInquiry != nil {
		body["pending_inquiry"] = v.pendingInquiry
	}
	if v.turnsUsed > 0 {
		body["turns_used"] = v.turnsUsed
	}
	if len(body) == 0 {
		return nil
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil
	}
	return data
}

// run is the observer goroutine main loop. Folds events,
// applies debounce + force-emit policy, calls emitStatus when
// it's time.
func (v *sessionView) run() {
	defer v.wg.Done()
	// Implemented in fold.go to keep this file readable.
	v.loop()
}
