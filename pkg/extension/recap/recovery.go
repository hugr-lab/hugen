package recap

import (
	"context"
	"encoding/json"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

// Recover implements [extension.Recovery]. It rebuilds the recap handle
// of a restarted root session in two steps:
//
//  1. Seed the COMPRESSED recap + watermark from the LAST recap frame the
//     session emitted (latest-wins).
//  2. Rebuild the un-folded tail by replaying the user↔assistant messages
//     with seq PAST that watermark — the messages that hadn't been folded
//     when the session stopped — so the effective topic (recap ⊕ tail)
//     resumes intact.
//
// Best-effort + nil-safe: a non-root session (no handle) is a no-op; a
// row that doesn't decode is skipped.
func (e *Extension) Recover(_ context.Context, state extension.SessionState, events []store.EventRow) error {
	h := FromState(state)
	if h == nil {
		return nil
	}

	// Step 1 — last compressed recap + watermark.
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
	var watermark int64
	if last != nil {
		h.restore(Recap{
			Topic:            last.Topic,
			Text:             last.Text,
			Categories:       last.Categories,
			ChangeConfidence: last.ChangeConfidence,
		}, last.WatermarkSeq)
		watermark = last.WatermarkSeq
	}

	// Step 2 — rebuild the tail from messages past the watermark.
	for _, r := range events {
		seq := int64(r.Seq)
		if seq <= watermark {
			continue
		}
		switch protocol.Kind(r.EventType) {
		case protocol.KindUserMessage:
			h.appendTurn(seq, "user", r.Content, e.maxMsgChars, e.windowCapChars)
		case protocol.KindAgentMessage:
			h.appendTurn(seq, "assistant", r.Content, e.maxMsgChars, e.windowCapChars)
		}
	}
	return nil
}
