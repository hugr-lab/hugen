package session

import (
	"context"
	"fmt"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// RequestInquiry is the runtime-side counterpart to the
// `session:inquire` tool. Unlike the tool path (model-initiated,
// turn-blocking, registered as a ToolFeed) this helper is called
// directly by goroutine-driven runtime components — currently
// pkg/extension/mission's planner loop, which needs to issue an
// approval inquire FROM the mission session itself after parsing
// the planner's handoff (so the question can be rendered
// deterministically from the typed plan body instead of relying
// on the model to embed the roadmap in free-text).
//
// Behaviour:
//   - Emits an InquiryRequest on the session's outbox. The frame
//     bubbles up the parent chain via the usual subagent_pump
//     projection and is surfaced to the user by the adapter.
//   - Blocks until the matching InquiryResponse arrives via
//     dispatchInquiryResponse (RouteInternal handler for
//     KindInquiryResponse), the per-call deadline fires, or ctx
//     cancels.
//   - Times out using the same default the inquire tool uses
//     (resolveInquireTimeout). The caller may override by passing
//     a non-zero payload.TimeoutMs.
//   - Returns the InquiryResponse on success; (nil, error) on
//     timeout / ctx cancel / closed session.
//
// CallerSessionID + RequestID on the emitted payload are
// authoritative — the caller may NOT pre-set RequestID; the
// helper allocates a fresh id and threads it through both the
// pending-channel registration and the frame body so the
// dispatcher's matching path works.
//
// Unlike the tool path this helper does NOT register a ToolFeed
// — the mission session is not blocked on a tool when the
// runtime calls this. Liveview still surfaces the in-flight
// inquire via snapshotPendingInquiry (driven by the pending map
// the helper populates), so the adapter renders the approval
// modal as usual.
func (s *Session) RequestInquiry(ctx context.Context, payload protocol.InquiryRequestPayload) (*protocol.InquiryResponse, error) {
	if s == nil {
		return nil, fmt.Errorf("session: RequestInquiry: nil session")
	}
	if s.IsClosed() {
		return nil, fmt.Errorf("session: RequestInquiry: session has terminated")
	}
	if payload.Type == "" {
		return nil, fmt.Errorf("session: RequestInquiry: payload.Type is required")
	}
	if payload.Question == "" {
		return nil, fmt.Errorf("session: RequestInquiry: payload.Question is required")
	}

	timeoutMs := payload.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = s.resolveInquireTimeout()
	}
	requestID := newInquiryRequestID()
	startedAt := time.Now().UTC()

	ref := &protocol.PendingInquiryRef{
		RequestID: requestID,
		Type:      payload.Type,
		Question:  payload.Question,
		StartedAt: startedAt,
	}
	respCh := s.recordPending(requestID, ref)
	defer s.clearPending(requestID)

	out := payload
	out.RequestID = requestID
	out.CallerSessionID = s.id
	out.TimeoutMs = timeoutMs

	req := protocol.NewInquiryRequest(s.id, s.agent.Participant(), out)
	if err := s.emit(ctx, req); err != nil {
		return nil, fmt.Errorf("session: RequestInquiry: emit: %w", err)
	}

	deadline := time.NewTimer(time.Duration(timeoutMs) * time.Millisecond)
	defer deadline.Stop()

	select {
	case resp := <-respCh:
		return resp, nil
	case <-deadline.C:
		select {
		case resp := <-respCh:
			return resp, nil
		default:
		}
		return nil, fmt.Errorf("session: RequestInquiry: deadline after %dms", timeoutMs)
	case <-ctx.Done():
		select {
		case resp := <-respCh:
			return resp, nil
		default:
		}
		return nil, fmt.Errorf("session: RequestInquiry: %w", ctx.Err())
	}
}
