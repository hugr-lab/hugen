package tui

import (
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

// replayLimit caps the number of historical events stitched into a
// freshly attached tab. 100 events keeps the chat pane scrollable
// without dragging weeks of context into memory; the operator can
// page back via the live runtime if they need more.
const replayLimit = 100

// replayEvents feeds persisted events through the same handleFrame
// pipeline a live tab uses for new frames. Event ordering follows
// the store's natural seq ASC. Non-chat events (tool_call,
// reasoning, extension frames, …) flow through too — handleFrame
// already filters them appropriately and the latest liveview/status
// will populate the sidebar.
//
// On any per-event decode error the offending row is logged-and-
// skipped rather than aborting the whole replay; a partially
// recovered chat is more useful than a blank one.
func replayEvents(t *tab, rows []store.EventRow) {
	for _, row := range rows {
		f, err := store.EventRowToFrame(row)
		if err != nil {
			if t.logger != nil {
				t.logger.Warn("tui: replay decode skip",
					"session", t.sessionID,
					"event_id", row.ID,
					"kind", row.EventType,
					"err", err)
			}
			continue
		}
		// Live handleFrame drops UserMessage because the live path
		// echoes the bubble via appendUserBubble on submit — but
		// during replay there IS no parallel submit, so the
		// persisted UserMessage is our only source of truth for
		// the user's side of the transcript. Project explicitly
		// before handing off to handleFrame so other kinds (agent
		// messages, reasoning, tool calls, …) still flow through
		// their normal branches.
		switch v := f.(type) {
		case *protocol.UserMessage:
			label := v.Author().Name
			if label == "" {
				label = v.Author().ID
			}
			if label == "" {
				label = "user"
			}
			t.chat.appendUser(label, v.Payload.Text)
			continue
		case *protocol.AgentMessage:
			// Consolidated+Final rows carry the full assistant
			// text — synthesise the span directly. Live mode
			// uses finalizeAssistant() to flush an accumulator
			// that's empty during replay (no streaming chunks
			// in the persisted log because consolidation already
			// happened).
			if v.Payload.Consolidated && v.Payload.Final && v.Payload.Text != "" {
				t.chat.spans = append(t.chat.spans,
					chatSpan{kind: spanAssistant, label: "hugen", text: v.Payload.Text})
				continue
			}
			// Non-terminal consolidated rows (tool-iteration
			// markers) and pre-consolidation streaming chunks
			// are skipped: they encode in-flight state, not
			// the final transcript.
			continue
		}
		t.handleFrame(f)
	}
	// Drop the streaming "thinking…" if replay ended mid-turn —
	// the live subscription will re-stream the in-flight turn (if
	// any) and re-arm the indicator. Clean state is friendlier.
	if t.chat.pendingAssistant.Len() > 0 {
		t.chat.pendingAssistant.Reset()
	}
	if t.chat.pendingReasoning.Len() > 0 {
		t.chat.finalizeReasoning()
	}
	t.statusLine = "ready"
	t.refreshChat()
}
