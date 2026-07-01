package a2a

import (
	"context"
	"fmt"
	"time"

	"github.com/hugr-lab/hugen/pkg/hugenclient"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// clientFrameIO is the executor's frameIO backed by the native HTTP API. It
// maps the runtime's frame-level operations onto the API's typed endpoints:
// a UserMessage → SendMessage, an InquiryResponse → AnswerInquiry, a Cancel →
// Cancel; Subscribe → a live-only SSE stream adapted back to a frame channel.
// This is the seam swap that moves the A2A bridge out-of-process: the
// translation logic above it (executor / registry / inquiry) is unchanged.
type clientFrameIO struct {
	client *hugenclient.Client
}

var _ frameIO = clientFrameIO{}

// Submit routes an inbound frame to the matching API endpoint. The frame's
// author is set API-side (from the bridge's token), so only the payload is sent.
func (io clientFrameIO) Submit(ctx context.Context, f protocol.Frame) error {
	switch fr := f.(type) {
	case *protocol.UserMessage:
		return io.client.SendMessage(ctx, fr.SessionID(), fr.Payload.Text)
	case *protocol.InquiryResponse:
		return io.client.AnswerInquiry(ctx, fr.SessionID(), toInquiryAnswer(fr.Payload))
	case *protocol.Cancel:
		return io.client.Cancel(ctx, fr.SessionID(), fr.Payload.Cascade)
	default:
		return fmt.Errorf("a2a: clientFrameIO cannot submit frame of kind %s", f.Kind())
	}
}

// Subscribe opens a live-only stream (no history replay — the bridge submits a
// fresh turn and wants only its frames) and adapts the StreamEvent channel to
// the plain frame channel the executor drains. The goroutine ends when the
// stream closes (ctx cancel on turn end) or errors.
func (io clientFrameIO) Subscribe(ctx context.Context, sessionID string) (<-chan protocol.Frame, error) {
	se, err := io.client.StreamLive(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	out := make(chan protocol.Frame)
	go func() {
		defer close(out)
		for ev := range se {
			if ev.Err != nil {
				return
			}
			select {
			case out <- ev.Frame:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// CloseSession terminates the durable root (Cancel cascades a real stop).
func (io clientFrameIO) CloseSession(ctx context.Context, sessionID, _ string) (time.Time, error) {
	return time.Time{}, io.client.CloseSession(ctx, sessionID)
}

// toInquiryAnswer maps the runtime inquiry payload to the client's request body.
func toInquiryAnswer(p protocol.InquiryResponsePayload) hugenclient.InquiryAnswer {
	return hugenclient.InquiryAnswer{
		RequestID:        p.RequestID,
		Approved:         p.Approved,
		Response:         p.Response,
		Reason:           p.Reason,
		Answers:          p.Answers,
		AutoApproveTools: p.AutoApproveTools,
	}
}
