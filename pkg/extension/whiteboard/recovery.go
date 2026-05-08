package whiteboard

import (
	"context"
	"encoding/json"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

// Recover implements [extension.Recovery]. Walks the session's full
// event log forward; for each [protocol.KindExtensionFrame] addressed
// to the whiteboard extension (CategoryOp), decodes the payload and
// applies it to the projection via [Apply]. The host writes Op=init
// / write / stop on its own session; members write Op=write on
// receipt of a broadcast — both surface here as a single CategoryOp
// stream owned by this session.
//
// Errors decoding a single row are silently skipped (spec §8 — replay
// is best-effort; unknown / malformed rows must not abort startup).
func (e *Extension) Recover(_ context.Context, state extension.SessionState, events []store.EventRow) error {
	h := FromState(state)
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ev := range events {
		if protocol.Kind(ev.EventType) != protocol.KindExtensionFrame {
			continue
		}
		op, payload, ok := decodeOp(ev)
		if !ok {
			continue
		}
		switch op {
		case OpInit, OpStop:
			h.wb = Apply(h.wb, ProjectEvent{Op: op, At: ev.CreatedAt})
		case OpWrite:
			h.wb = Apply(h.wb, ProjectEvent{
				Op:            OpWrite,
				Seq:           payload.Seq,
				At:            ev.CreatedAt,
				FromSessionID: payload.FromSessionID,
				FromRole:      payload.FromRole,
				Text:          payload.Text,
				Truncated:     payload.Truncated,
			})
		}
	}
	return nil
}

// decodeOp pulls (op, writeData) from an ExtensionFrame row whose
// owning extension is "whiteboard". Returns ok=false for rows that
// belong to another extension or fail to decode — Recovery silently
// skips those rather than abort.
//
// store.FrameToEventRow flattens [protocol.ExtensionFramePayload]
// onto the metadata map (extension/category/op/data as top-level
// keys), which is the shape this decoder reads.
func decodeOp(ev store.EventRow) (string, writeData, bool) {
	if ev.Metadata == nil {
		return "", writeData{}, false
	}
	ext, _ := ev.Metadata["extension"].(string)
	if ext != providerName {
		return "", writeData{}, false
	}
	op, _ := ev.Metadata["op"].(string)
	if op == "" {
		return "", writeData{}, false
	}
	if op != OpWrite {
		return op, writeData{}, true
	}
	raw, has := ev.Metadata["data"]
	if !has {
		return op, writeData{}, true
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return op, writeData{}, true
	}
	var d writeData
	if err := json.Unmarshal(b, &d); err != nil {
		return op, writeData{}, true
	}
	return op, d, true
}
