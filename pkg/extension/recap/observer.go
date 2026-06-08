package recap

import (
	"context"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// OnFrameEmit implements [extension.FrameObserver]. It appends each root
// user↔assistant message to the bounded recent-message ring.
//
// The append is freshness-critical and MUST stay synchronous (a fast
// in-memory op under the recap mutex) — never offloaded to a goroutine.
// notifyFrameObservers runs this inline within Session.emit, and startTurn
// emits the user message BEFORE it renders the turn, so a synchronous
// append is what guarantees the marker the boundary fold forms carries the
// latest user message when the skill advertise reads it. The (model-
// calling) FOLD does NOT run here: it runs at [Extension.OnTurnBoundary].
// No-op for non-root sessions (FromState nil).
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
	switch f := frame.(type) {
	case *protocol.UserMessage:
		// Skip system-synthetic user messages (author=agent) — e.g. the
		// async-mission summary kick (kickAsyncSummaryTurn). Those aren't
		// real conversation; the mission's findings reach the marker via
		// the agent's summary reply (an AgentMessage) at the next turn.
		if f.Author().Kind == protocol.ParticipantAgent {
			return
		}
		h.appendMessage("user", f.Payload.Text, e.maxMsgChars, e.maxRing)
	case *protocol.AgentMessage:
		if f.Payload.Consolidated && f.Payload.Final {
			h.appendMessage("assistant", f.Payload.Text, e.maxMsgChars, e.maxRing)
		}
	}
}

// OnTurnBoundary implements [extension.TurnBoundaryHook]. It runs
// SYNCHRONOUSLY at the idle→active boundary (Session.startTurn, after the
// user message has been emitted + appended to the ring, before the turn
// renders) and (re)forms the topic marker via the cheap model — every
// turn. So the marker is current before the skill advertise reads it as
// its retrieval anchor, instead of lagging behind the conversation.
//
// The turn waits on the (cheap, small-input) summariser, bounded by
// BuildTimeout — "completes or times out", either way the turn proceeds
// (the raw recent ring still backs CurrentRecap if the fold didn't land).
// No-op for non-root sessions (FromState nil) and turns with no new user
// message (the fold short-circuits).
func (e *Extension) OnTurnBoundary(ctx context.Context, state extension.SessionState) error {
	h := FromState(state)
	if h == nil {
		return nil
	}
	if h.beginRefresh() {
		e.fold(ctx, state, h)
	}
	return nil
}
