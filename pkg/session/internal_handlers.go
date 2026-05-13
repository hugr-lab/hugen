package session

import (
	"context"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// internalHandler runs a synchronous side-effect for a RouteInternal
// Frame. The handler may emit further Frames (via s.emit) or trigger
// downstream side-effects (e.g. m.Submit to siblings); the Frame
// itself never lands in s.history or s.pendingInbound. Errors are
// logged inside the handler — the dispatcher's contract is "best
// effort, fire-and-forget".
//
// Stage-7.2 (whiteboard ext) made [protocol.KindExtensionFrame] the
// only registered RouteInternal kind; dispatchExtensionFrame walks
// the registered extensions for an [extension.FrameRouter] match.
// Phase 5 may register HITL forwarding handlers here as well.
type internalHandler func(s *Session, ctx context.Context, f protocol.Frame)

// internalHandlers maps each RouteInternal Frame Kind to its sync
// side-effect handler. Kept package-level + immutable after init so
// the hot path doesn't need a lock: register at init time, read on
// every routed frame.
var internalHandlers = map[protocol.Kind]internalHandler{
	protocol.KindExtensionFrame:  dispatchExtensionFrame,
	protocol.KindInquiryResponse: dispatchInquiryResponse,
}

// dispatchInternal looks up and invokes the registered handler for
// the Frame's Kind. No-op (with a debug log) when no handler is
// registered — spec §10.2 says RouteInternal Frames "trigger a
// runtime side-effect that does not need to reach the model", so a
// missing handler means the routing table is wrong, not that the
// Frame should fall through to history.
func (s *Session) dispatchInternal(ctx context.Context, f protocol.Frame) {
	if h, ok := internalHandlers[f.Kind()]; ok {
		h(s, ctx, f)
		return
	}
	s.logger.Debug("session: RouteInternal frame with no handler registered",
		"session", s.id, "kind", f.Kind())
}

// dispatchExtensionFrame looks up the [extension.FrameRouter]
// matching the inbound [protocol.ExtensionFrame]'s Extension field
// and invokes HandleFrame. Extensions without a FrameRouter (plan,
// notepad, skill) never receive cross-session ExtensionFrames; an
// inbound frame addressed to one of them is logged at debug and
// dropped so a routing typo surfaces in the log without panicking
// the session.
func dispatchExtensionFrame(s *Session, ctx context.Context, f protocol.Frame) {
	ef, ok := f.(*protocol.ExtensionFrame)
	if !ok {
		return
	}
	name := ef.Payload.Extension
	if s.deps == nil {
		return
	}
	for _, ext := range s.deps.Extensions {
		if ext.Name() != name {
			continue
		}
		router, ok := ext.(extension.FrameRouter)
		if !ok {
			s.logger.Debug("session: extension has no FrameRouter",
				"session", s.id, "extension", name, "op", ef.Payload.Op)
			return
		}
		if err := router.HandleFrame(ctx, s, ef); err != nil {
			s.logger.Warn("session: extension framerouter",
				"session", s.id, "extension", name, "op", ef.Payload.Op, "err", err)
		}
		return
	}
	s.logger.Debug("session: ExtensionFrame addressed to unknown extension",
		"session", s.id, "extension", name, "op", ef.Payload.Op)
}
