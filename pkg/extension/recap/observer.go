package recap

import (
	"context"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// OnFrameEmit implements [extension.FrameObserver]. It appends each root
// user↔assistant message to the un-folded tail.
//
// The append is freshness-critical and MUST stay synchronous (a fast
// in-memory op under the recap mutex) — never offloaded to a goroutine.
// notifyFrameObservers runs this inline within Session.emit, and
// startTurn emits the user message BEFORE it renders the turn, so a
// synchronous append is what guarantees the effective topic (recap ⊕
// tail) carries the latest user message when the skill advertise reads
// it. The (model-calling) FOLD does NOT run here: it moved to
// [Extension.OnTurnBoundary] so the session can wait on it deterministically
// between the user-message emit and the render. No-op for non-root
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
	case *protocol.AgentMessage:
		if f.Payload.Consolidated && f.Payload.Final {
			h.appendTurn(seq, "assistant", f.Payload.Text, e.maxMsgChars, e.windowCapChars)
		}
	}
}

// OnTurnBoundary implements [extension.TurnBoundaryHook]. It runs
// SYNCHRONOUSLY at the idle→active boundary (Session.startTurn, after the
// user message has been emitted + appended to the tail, before the turn
// renders). When the tail has crossed the fold threshold it folds it into
// the compressed recap RIGHT HERE — so the compressed recap, topic, and
// change_confidence are current before the skill advertise reads them as
// its retrieval query + cache-refresh gate, instead of lagging a turn
// behind an async fold.
//
// This is a deliberate trade against the slice-3 non-blocking design: the
// turn waits on the (cheap summarizer) fold, but only on the turns where
// the tail actually crossed the threshold (every few turns, not every
// turn), and the fold is bounded by BuildTimeout — "completes or times
// out", either way the turn proceeds with a usable recap (effective topic
// = recap ⊕ tail stays valid even if the fold didn't land). No-op for
// non-root sessions (FromState nil) and below-threshold turns.
func (e *Extension) OnTurnBoundary(ctx context.Context, state extension.SessionState) error {
	h := FromState(state)
	if h == nil {
		return nil
	}
	if h.needsFold(e.foldThresholdChars) && h.beginRefresh() {
		e.fold(ctx, state, h)
	}
	return nil
}
