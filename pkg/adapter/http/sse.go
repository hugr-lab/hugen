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
// Frames in the live channel are deduped against maxReplayedSeq:
// any frame with seq <= maxReplayedSeq has already been written
// to the consumer during replay (the agent emitted it after
// Subscribe registered but before the replay query finished —
// the registration-before-replay ordering captures it on `live`,
// and persistence puts it in the replay rows; the dedupe step
// resolves the overlap).
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
	maxReplayedSeq := 0
	for _, row := range replay {
		f, err := runtime.EventRowToFrame(row)
		if err != nil {
			a.logger.Warn("sse: failed to materialise replay row",
				"session", sessionID, "seq", row.Seq, "err", err)
			continue
		}
		if err := a.writeFrameEvent(w, flusher, f); err != nil {
			return
		}
		if row.Seq > maxReplayedSeq {
			maxReplayedSeq = row.Seq
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
			// Dedupe: a frame emitted between Subscribe and replay
			// completion appears in both. The replay path wrote it
			// already; drop it here.
			if maxReplayedSeq > 0 && f.Seq() > 0 && f.Seq() <= maxReplayedSeq {
				continue
			}
			if err := a.writeFrameEvent(w, flusher, f); err != nil {
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

// writeFrameEvent writes one SSE event for a Frame:
//
//	id: <seq>
//	event: <kind>
//	data: <json envelope>
//
// followed by a blank line. Flushes after the terminator. The id
// line is always emitted when the frame carries a non-zero seq
// (Session.emit assigns one before push to the outbox, and
// EventRowToFrame propagates the persisted seq on replay). A zero
// seq means the frame was constructed but never persisted —
// shouldn't occur on the SSE wire; we still emit it without an id
// so a defensive caller doesn't crash.
func (a *Adapter) writeFrameEvent(w stdhttp.ResponseWriter, flusher stdhttp.Flusher, f protocol.Frame) error {
	data, err := a.codec.EncodeFrame(f)
	if err != nil {
		a.logger.Warn("sse: encode frame", "kind", f.Kind(), "err", err)
		return err
	}
	var buf bytes.Buffer
	if seq := f.Seq(); seq > 0 {
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
