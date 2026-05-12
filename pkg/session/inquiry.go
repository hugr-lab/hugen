package session

import (
	"context"
	"sync"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// inquiryState owns the two per-session maps the phase-5.1 HITL
// machinery needs:
//
//   - pending: RequestID → channel the inquire tool blocks on for
//     this session. Populated by the inquire tool body before it
//     emits its InquiryRequest; cleared on response delivery, tool
//     ctx cancel, or timeout. Read from the active tool feed's
//     Feed callback (single channel write per response).
//   - routing: RequestID → direct-child session id the ancestor
//     pump bubbled this RequestID up from. Populated by the pump
//     each time it observes a child's *InquiryRequest. Read by
//     dispatchInquiryResponse on the way down. Cleared on response
//     delivery, on child termination (sweep), or on caller cancel
//     (best-effort).
//
// Each map has its own small mutex so the two access paths
// (tool body / pump goroutine / Run loop) don't contend. The
// state lives in a dedicated struct so the Session struct stays
// readable; zero value is ready to use after the first
// initInquiry call (lazy init guards via the mutex).
type inquiryState struct {
	pendingMu sync.Mutex
	pending   map[string]chan *protocol.InquiryResponse

	routingMu sync.Mutex
	routing   map[string]string
}

// recordPending registers the channel the inquire tool body
// blocks on for the given RequestID. The channel is buffered to
// 1 so the dispatcher's Feed callback never blocks; multiple
// arrivals for the same RequestID drop after the first.
func (s *Session) recordPending(requestID string) chan *protocol.InquiryResponse {
	ch := make(chan *protocol.InquiryResponse, 1)
	s.inquiry.pendingMu.Lock()
	defer s.inquiry.pendingMu.Unlock()
	if s.inquiry.pending == nil {
		s.inquiry.pending = make(map[string]chan *protocol.InquiryResponse)
	}
	s.inquiry.pending[requestID] = ch
	return ch
}

// clearPending removes the entry for RequestID. Called from the
// tool body's defer once it returns (whether by response, ctx,
// or timeout) so a late response after that point cannot deliver
// to a stale channel.
func (s *Session) clearPending(requestID string) {
	s.inquiry.pendingMu.Lock()
	defer s.inquiry.pendingMu.Unlock()
	delete(s.inquiry.pending, requestID)
}

// deliverPending pushes resp to the channel registered for the
// caller's RequestID, dropping silently when no entry exists
// (caller already returned, race with timeout / cancel). Returns
// true on successful delivery.
func (s *Session) deliverPending(resp *protocol.InquiryResponse) bool {
	s.inquiry.pendingMu.Lock()
	ch, ok := s.inquiry.pending[resp.Payload.RequestID]
	s.inquiry.pendingMu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- resp:
		return true
	default:
		// Buffered channel of 1 — already filled by an earlier
		// delivery (idempotency-on-restart can re-emit). Treat as
		// success: the tool already has its answer.
		return true
	}
}

// recordResponseRoute is the pump's per-hop bookkeeping —
// "when a response with this RequestID comes back through me,
// forward it to this child". Phase 5.1 § 2.3.
func (s *Session) recordResponseRoute(requestID, childID string) {
	s.inquiry.routingMu.Lock()
	defer s.inquiry.routingMu.Unlock()
	if s.inquiry.routing == nil {
		s.inquiry.routing = make(map[string]string)
	}
	s.inquiry.routing[requestID] = childID
}

// lookupResponseRoute returns the direct-child id the
// dispatchInquiryResponse handler should forward the response to,
// and false when no route exists at this hop.
func (s *Session) lookupResponseRoute(requestID string) (string, bool) {
	s.inquiry.routingMu.Lock()
	defer s.inquiry.routingMu.Unlock()
	cid, ok := s.inquiry.routing[requestID]
	return cid, ok
}

// clearResponseRoute drops the route entry for RequestID. Called
// from dispatchInquiryResponse after a successful forward, from
// the timeout / ctx-cancel cleanup path on the originator, and
// from sweepResponseRoutesForChild when a child terminates.
func (s *Session) clearResponseRoute(requestID string) {
	s.inquiry.routingMu.Lock()
	defer s.inquiry.routingMu.Unlock()
	delete(s.inquiry.routing, requestID)
}

// sweepResponseRoutesForChild removes every routing entry whose
// value points at the terminated child. Called from the
// child-deregister path so late responses for an inquire that
// the terminated child's descendant had in flight surface as
// "no route" at this hop and drop with warn instead of being
// forwarded to a closed inbox. Phase 5.1 § 2.4.
func (s *Session) sweepResponseRoutesForChild(childID string) {
	s.inquiry.routingMu.Lock()
	defer s.inquiry.routingMu.Unlock()
	for rid, cid := range s.inquiry.routing {
		if cid == childID {
			delete(s.inquiry.routing, rid)
		}
	}
}

// dispatchInquiryResponse is the RouteInternal handler for
// KindInquiryResponse. Phase 5.1 § 2.4:
//
//   - If CallerSessionID == s.id, the response has reached the
//     hop that called inquire. Deliver it to the pending channel
//     (which the tool body is blocked on) and clear local state.
//   - Else look up the per-RequestID route. If found, forward to
//     the direct child via Submit; clear the route. If not found
//     (terminated chain, cleared by timeout / cancel), drop with
//     warn.
//
// The handler is sync — RouteInternal runs in the Run loop, the
// forward is a Submit (channel send), no blocking. Phase 5.1's
// "routing-only frames are safe inline" principle holds.
func dispatchInquiryResponse(s *Session, ctx context.Context, f protocol.Frame) {
	resp, ok := f.(*protocol.InquiryResponse)
	if !ok {
		return
	}
	rid := resp.Payload.RequestID
	if resp.Payload.CallerSessionID == s.id {
		if delivered := s.deliverPending(resp); !delivered {
			s.logger.Warn("session: inquiry_response with no pending caller; drop",
				"session", s.id, "request_id", rid)
		}
		s.clearResponseRoute(rid)
		return
	}
	cid, ok := s.lookupResponseRoute(rid)
	if !ok {
		s.logger.Warn("session: inquiry_response at hop with no route; drop",
			"session", s.id, "request_id", rid,
			"caller_session_id", resp.Payload.CallerSessionID)
		return
	}
	s.childMu.Lock()
	child, present := s.children[cid]
	s.childMu.Unlock()
	if !present || child == nil {
		s.logger.Warn("session: inquiry_response forward target gone; drop",
			"session", s.id, "request_id", rid, "child", cid)
		s.clearResponseRoute(rid)
		return
	}
	// Submit is non-blocking-ish — it spawns a delivery goroutine
	// that owns the inbox channel send with closed-channel recovery.
	// Fire-and-forget here; the response either lands or the chain
	// at the next hop logs and drops.
	_ = child.Submit(ctx, resp)
	s.clearResponseRoute(rid)
}
