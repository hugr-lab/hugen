package session

import (
	"context"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// internalHandler runs a synchronous side-effect for a RouteInternal
// Frame. The handler may emit further Frames (via s.emit) or trigger
// downstream side-effects (e.g. m.Deliver to siblings); the Frame
// itself never lands in s.history or s.pendingInbound. Errors are
// logged inside the handler — the dispatcher's contract is "best
// effort, fire-and-forget".
//
// Phase 4 step 10 (whiteboard primitive) populates this map with
// host-side broadcast handlers; C6 leaves it empty so the routing
// plumbing exists ahead of the kinds it'll dispatch.
type internalHandler func(s *Session, ctx context.Context, f protocol.Frame)

// internalHandlers maps each RouteInternal Frame Kind to its sync
// side-effect handler. C6 registers no entries — phase-4 step 10
// fills in whiteboard_write@host / whiteboard_stop@host handlers and
// the matching Kind constants in pkg/protocol. Phase 5 may register
// HITL forwarding handlers here as well.
//
// Kept package-level + immutable after init so the hot path doesn't
// need a lock: register at init time, read on every routed frame.
var internalHandlers = map[protocol.Kind]internalHandler{}

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
