package recap

import (
	"context"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// OnFrameEmit implements [extension.FrameObserver]. It appends each root
// user↔assistant message to the un-folded tail and, on a user turn,
// folds the tail into the compressed recap if it has grown past the
// threshold.
//
// Non-blocking on the hot path: tail appends are in-memory; the fold
// (model call) runs on a detached goroutine (one at a time, guarded by
// beginRefresh) so the conversation never waits on it. The effective
// topic (recap ⊕ tail) stays usable throughout. No-op for non-root
// sessions (FromState nil).
//
// Both sides are captured: a user turn is often a pointer into the
// assistant's prior turn, so the topic lives across the pair. Only
// CONSOLIDATED final agent messages are recorded — streaming chunks are
// outbox-only and would duplicate the reply.
func (e *Extension) OnFrameEmit(_ context.Context, state extension.SessionState, frame protocol.Frame) {
	h := FromState(state)
	if h == nil {
		return
	}
	seq := int64(frame.Seq())
	switch f := frame.(type) {
	case *protocol.UserMessage:
		h.appendTurn(seq, "user", f.Payload.Text, e.maxMsgChars, e.windowCapChars)
		// A user message is the per-cycle fold checkpoint. Fold only when
		// recap ⊕ tail crossed the threshold, and only one at a time.
		if h.needsFold(e.foldThresholdChars) && h.beginRefresh() {
			go e.fold(state, h)
		}
	case *protocol.AgentMessage:
		if f.Payload.Consolidated && f.Payload.Final {
			h.appendTurn(seq, "assistant", f.Payload.Text, e.maxMsgChars, e.windowCapChars)
		}
	}
}
