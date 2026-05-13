package session

import (
	"context"
	"testing"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// TestDispatchInquiryResponse_DeliverToOriginator covers the
// branch where CallerSessionID == s.id: the response lands on
// the pending channel and any local route entry is cleared.
func TestDispatchInquiryResponse_DeliverToOriginator(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	const requestID = "req-deliver-1"
	ch := parent.recordPending(requestID, nil)
	defer parent.clearPending(requestID)

	// Defensive route entry — dispatcher should clear it after
	// delivery so a late re-emit does not double-forward.
	parent.recordResponseRoute(requestID, "ses-stale-child")

	resp := protocol.NewInquiryResponse(parent.id, protocol.ParticipantInfo{},
		protocol.InquiryResponsePayload{
			RequestID:       requestID,
			CallerSessionID: parent.id,
			Response:        "ok",
		})
	dispatchInquiryResponse(parent, context.Background(), resp)

	select {
	case got := <-ch:
		if got.Payload.Response != "ok" {
			t.Errorf("delivered payload: got %q, want \"ok\"", got.Payload.Response)
		}
	default:
		t.Fatal("response did not land on pending channel")
	}
	if _, ok := parent.lookupResponseRoute(requestID); ok {
		t.Errorf("route entry not cleared after originator delivery")
	}
}

// TestDispatchInquiryResponse_NoRouteDrops covers the case where
// the response cascaded into a hop with no matching route
// (chain previously cleared by timeout / sibling teardown). The
// handler logs and drops without panicking.
func TestDispatchInquiryResponse_NoRouteDrops(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	resp := protocol.NewInquiryResponse(parent.id, protocol.ParticipantInfo{},
		protocol.InquiryResponsePayload{
			RequestID:       "req-orphan-1",
			CallerSessionID: "ses-some-other-originator",
			Response:        "nope",
		})
	// No recordResponseRoute, no recordPending → both lookups miss.
	// We just verify no panic and no state leak.
	dispatchInquiryResponse(parent, context.Background(), resp)

	if _, ok := parent.lookupResponseRoute("req-orphan-1"); ok {
		t.Errorf("orphan dispatch wrote a route entry")
	}
}

// TestDispatchInquiryResponse_ChildGoneClearsRoute covers the
// branch where a route exists but the child has been removed
// from the parent's children map (terminated mid-flight). The
// handler must clear the route and not crash on the nil child.
func TestDispatchInquiryResponse_ChildGoneClearsRoute(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	const requestID = "req-gone-1"
	const childID = "ses-already-gone"

	parent.recordResponseRoute(requestID, childID)

	// Simulate "child gone": children map either doesn't have the
	// entry, or has a nil value. Both must be handled.
	parent.childMu.Lock()
	if parent.children == nil {
		parent.children = make(map[string]*Session)
	}
	delete(parent.children, childID)
	parent.childMu.Unlock()

	resp := protocol.NewInquiryResponse(parent.id, protocol.ParticipantInfo{},
		protocol.InquiryResponsePayload{
			RequestID:       requestID,
			CallerSessionID: "ses-deep-originator",
			Response:        "x",
		})
	dispatchInquiryResponse(parent, context.Background(), resp)

	if _, ok := parent.lookupResponseRoute(requestID); ok {
		t.Errorf("route not cleared after target-gone branch")
	}
}

// TestSweepResponseRoutesForChild covers the child-teardown hook
// that wipes every routing entry pointing at a terminated child.
// Entries for other children must survive.
func TestSweepResponseRoutesForChild(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	parent.recordResponseRoute("rid-A1", "ses-child-A")
	parent.recordResponseRoute("rid-A2", "ses-child-A")
	parent.recordResponseRoute("rid-B1", "ses-child-B")

	parent.sweepResponseRoutesForChild("ses-child-A")

	if _, ok := parent.lookupResponseRoute("rid-A1"); ok {
		t.Errorf("sweep left rid-A1 behind")
	}
	if _, ok := parent.lookupResponseRoute("rid-A2"); ok {
		t.Errorf("sweep left rid-A2 behind")
	}
	if cid, ok := parent.lookupResponseRoute("rid-B1"); !ok || cid != "ses-child-B" {
		t.Errorf("sweep wiped sibling: ok=%v cid=%q", ok, cid)
	}
}

// TestDeliverPending_BufferedSecondDelivery covers the idempotent
// path: a duplicate response for the same RequestID lands silently
// (no panic, no deadlock) because the channel cap is 1.
func TestDeliverPending_BufferedSecondDelivery(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	const requestID = "req-dup-1"
	ch := parent.recordPending(requestID, nil)
	defer parent.clearPending(requestID)

	resp := protocol.NewInquiryResponse(parent.id, protocol.ParticipantInfo{},
		protocol.InquiryResponsePayload{RequestID: requestID, CallerSessionID: parent.id, Response: "first"})

	if !parent.deliverPending(resp) {
		t.Fatal("first deliverPending returned false")
	}
	if !parent.deliverPending(resp) {
		t.Fatal("second deliverPending returned false (must be idempotent)")
	}

	// First message in buffer, second silently dropped.
	select {
	case got := <-ch:
		if got.Payload.Response != "first" {
			t.Errorf("got %q, want first", got.Payload.Response)
		}
	default:
		t.Fatal("channel empty after deliverPending")
	}
	select {
	case got := <-ch:
		t.Errorf("second delivery leaked into channel: %+v", got)
	default:
	}
}
