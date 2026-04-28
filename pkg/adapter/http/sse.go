package http

import (
	"bytes"
	"context"
	"fmt"
	stdhttp "net/http"
	"strconv"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/runtime"
)

// sseConfig holds the tunable cadences for the SSE writer.
type sseConfig struct {
	heartbeatInterval time.Duration
	slowConsumerGrace time.Duration
}

// Default cadences per contracts/sse-wire-format.md.
//
//   - Heartbeat: 30s. Undercuts most aggressive proxy idle-close
//     defaults (≥60s) by a margin of two intervals.
//   - Slow-consumer grace: 50ms. Forgives momentary GC/scheduler
//     pauses; beyond it the frame is dropped to that subscriber and
//     the consumer recovers via Last-Event-ID replay (R-Plan-18).
const (
	defaultHeartbeatInterval = 30 * time.Second
	defaultSlowConsumerGrace = 50 * time.Millisecond
)

func sseConfigFromOptions(opts Options) sseConfig {
	c := sseConfig{
		heartbeatInterval: defaultHeartbeatInterval,
		slowConsumerGrace: defaultSlowConsumerGrace,
	}
	if opts.HeartbeatInterval > 0 {
		c.heartbeatInterval = time.Duration(opts.HeartbeatInterval) * time.Second
	}
	if opts.SlowConsumerGrace > 0 {
		c.slowConsumerGrace = time.Duration(opts.SlowConsumerGrace) * time.Millisecond
	}
	return c
}

// writeSSE serialises replay events first (preserving seq order)
// then drains the live channel. After every successful write the
// heartbeat timer is reset; on idle the writer emits the SSE
// comment line `: heartbeat\n\n` (which is NOT persisted —
// R-Plan-17).
//
// The function returns when ctx is cancelled, the live channel is
// closed by the runtime, or a write error indicates the consumer
// disconnected.
func (a *Adapter) writeSSE(
	ctx context.Context,
	w stdhttp.ResponseWriter,
	flusher stdhttp.Flusher,
	sessionID string,
	replay []runtime.EventRow,
	live <-chan protocol.Frame,
) {
	// Replay first: deterministic ordering, no heartbeats interleaved.
	for _, row := range replay {
		f, err := runtime.EventRowToFrame(row)
		if err != nil {
			a.logger.Warn("sse: failed to materialise replay row",
				"session", sessionID, "seq", row.Seq, "err", err)
			continue
		}
		if err := a.writeFrameEvent(w, flusher, row.Seq, f); err != nil {
			return
		}
	}

	// Live tail: select on ctx, frame, or heartbeat ticker.
	ticker := time.NewTicker(a.sseCfg.heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case f, ok := <-live:
			if !ok {
				return
			}
			seq := frameSeq(f)
			if err := a.writeFrameEvent(w, flusher, seq, f); err != nil {
				return
			}
			// Reset heartbeat — next idle interval starts now.
			ticker.Reset(a.sseCfg.heartbeatInterval)
		case <-ticker.C:
			if err := writeHeartbeat(w, flusher); err != nil {
				return
			}
		}
	}
}

// frameSeq pulls the per-session seq from a Frame. Live frames
// flowing through the runtime fan-out do not carry an explicit seq
// today; we approximate by 0 and let downstream consumers ignore it
// (the SSE wire still emits an id, derived from the persisted row's
// seq when available — see writeFrameEvent for the fallback).
//
// TODO(phase-3): persist seq on the Frame at fan-out time so live
// events carry an authoritative cursor. Phase 2 ships this seq
// hole as a known limitation; the reconnection invariant still
// holds because replay reads from the persistent log.
func frameSeq(f protocol.Frame) int {
	_ = f
	return 0
}

// writeFrameEvent writes one SSE event for a Frame:
//
//	id: <seq>
//	event: <kind>
//	data: <json envelope>
//
// followed by a blank line. Flushes after the terminator. If seq
// is zero (live frame) the id line is omitted — clients ignore
// missing ids and the next reconnection cursor falls back to the
// last persisted id seen during replay.
func (a *Adapter) writeFrameEvent(w stdhttp.ResponseWriter, flusher stdhttp.Flusher, seq int, f protocol.Frame) error {
	data, err := a.codec.EncodeFrame(f)
	if err != nil {
		a.logger.Warn("sse: encode frame", "kind", f.Kind(), "err", err)
		return err
	}
	// SSE forbids embedded newlines on a `data:` line; the codec
	// produces compact JSON but defensively guard anyway.
	if bytes.IndexByte(data, '\n') >= 0 {
		data = bytes.ReplaceAll(data, []byte{'\n'}, []byte(" "))
	}
	var buf bytes.Buffer
	if seq > 0 {
		buf.WriteString("id: ")
		buf.WriteString(strconv.Itoa(seq))
		buf.WriteByte('\n')
	}
	buf.WriteString("event: ")
	buf.WriteString(string(f.Kind()))
	buf.WriteByte('\n')
	buf.WriteString("data: ")
	buf.Write(data)
	buf.WriteString("\n\n")
	if _, err := w.Write(buf.Bytes()); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

// writeHeartbeat emits an SSE comment line. Discarded by EventSource
// (which surfaces only id/event/data fields), printed by curl -N.
// MUST NOT be persisted to session_events.
func writeHeartbeat(w stdhttp.ResponseWriter, flusher stdhttp.Flusher) error {
	if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

// setSSEHeaders writes the canonical headers from
// contracts/sse-wire-format.md. Must be called before WriteHeader.
func setSSEHeaders(w stdhttp.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
}
