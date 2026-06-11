package recap

import (
	"context"
	"encoding/json"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

// Recover implements [extension.Recovery]. It rebuilds the recap handle of
// a restarted root session in two steps:
//
//  1. Seed the latest marker from the LAST recap frame the session emitted
//     (latest-wins) — so a consumer reading before the next turn boundary
//     still gets the topic the session stopped on.
//  2. Rebuild the recent-message ring by replaying the user↔assistant
//     messages in order (the ring self-bounds to the most recent ones), so
//     the next boundary fold has context. The marker is re-formed at that
//     boundary regardless.
//
// Best-effort + nil-safe: a non-root session (no handle) is a no-op; a row
// that doesn't decode is skipped.
func (e *Extension) Recover(_ context.Context, state extension.SessionState, events []store.EventRow) error {
	h := FromState(state)
	if h == nil {
		return nil
	}

	// Step 1 — last marker.
	var last *framePayload
	for _, r := range events {
		if protocol.Kind(r.EventType) != protocol.KindExtensionFrame || r.Metadata == nil {
			continue
		}
		if ext, _ := r.Metadata["extension"].(string); ext != providerName {
			continue
		}
		if op, _ := r.Metadata["op"].(string); op != OpSet {
			continue
		}
		raw, ok := r.Metadata["data"]
		if !ok || raw == nil {
			continue
		}
		b, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		var fp framePayload
		if json.Unmarshal(b, &fp) == nil && fp.Text != "" {
			cp := fp // copy — &fp would alias the loop var
			last = &cp
		}
	}
	if last != nil {
		h.restore(Recap{
			Topic:      last.Topic,
			Text:       last.Text,
			Categories: last.Categories,
		})
	}

	// Step 2 — rebuild the recent-message ring (self-bounds to the tail).
	// Mirrors the live OnFrameEmit filter: on ROOT, agent-authored user
	// rows (the async-summary kick, a schedule wake — runtime synthetics
	// authored by the agent; EventRow.Author carries the participant ID)
	// are skipped, so a restart doesn't fold runtime boilerplate into the
	// topic. Agent-message rows need no consolidated check — the store
	// only persists consolidated finals.
	for _, r := range events {
		switch protocol.Kind(r.EventType) {
		case protocol.KindUserMessage:
			if h.root && e.deps.AgentID != "" && r.Author == e.deps.AgentID {
				continue
			}
			h.appendMessage("user", r.Content, e.maxMsgChars, e.maxRing)
		case protocol.KindAgentMessage:
			h.appendMessage("assistant", r.Content, e.maxMsgChars, e.maxRing)
		}
	}
	return nil
}
