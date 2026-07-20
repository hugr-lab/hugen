package httpapi

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

// sseCodec encodes frames to the wire JSON (stateless — shared).
var sseCodec = protocol.NewCodec()

const (
	// sseWriteTimeout bounds a single SSE write. A stuck client that blocks
	// longer is disconnected (the handler returns → r.Context() cancels → the
	// subscription is deregistered). The runtime then DRAINS the deregistered
	// channel (manager.Subscribe cleanup), so an in-flight blocking fanoutSend
	// completes instead of parking forever — that drain, not this deadline, is
	// what stops one dead reader from wedging the session's fanout. The deadline
	// just bounds how long a slow write stalls THIS handler.
	sseWriteTimeout = 15 * time.Second
	// sseHeartbeat keeps the connection alive through idle proxies and surfaces
	// a dead client (a failed heartbeat write ends the stream).
	sseHeartbeat = 15 * time.Second
)

// handleStream serves the session's frame stream over SSE. It subscribes FIRST
// (so frames emitted during replay are buffered), replays the persisted backlog
// from Last-Event-ID via ListEvents(MinSeq), then streams live — de-duplicating
// the overlap by seq. Multiple clients can open this on one session at once
// (the runtime's multi-subscriber fanout). H5.
func (a *Adapter) handleStream(w http.ResponseWriter, r *http.Request) {
	_, id, ok := a.ownedRequest(w, r)
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	// Revive a dormant session on attach. Opening the stream IS the attach
	// point, so resume the (root) session from the store if it isn't live —
	// otherwise a session that went dormant (process restart, idle GC) would
	// only replay a frozen backlog and silently swallow new messages (Submit
	// has no live session to route to). Reviving here restores live delivery
	// AND makes subsequent writes work. Idempotent: a live session returns
	// as-is. Uses the adapter's lifecycle ctx, NOT r.Context(), so the revived
	// session outlives this HTTP request. Best-effort: a non-root or closed
	// session can't be revived but can still be replayed read-only, so log and
	// continue rather than fail the stream.
	if _, rerr := a.host.ResumeSession(a.lifecycleCtx, id); rerr != nil {
		a.logger.Debug("httpapi: stream attach resume skipped", "id", id, "err", rerr)
	}

	// Subscribe before replay so nothing emitted during the replay window is
	// lost. The subscription lives for the request; client disconnect cancels
	// r.Context() → the runtime deregisters + the channel drains.
	sub, err := a.host.Subscribe(r.Context(), id)
	if err != nil {
		a.logger.Error("httpapi: subscribe", "id", id, "err", err)
		httpError(w, http.StatusInternalServerError, "subscribe failed")
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // disable proxy buffering (nginx)
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	rc := http.NewResponseController(w)

	// Replay the backlog after Last-Event-ID — unless ?live=1 (live-only): a
	// consumer that submits a fresh turn and wants ONLY that turn's frames (the
	// A2A bridge) must not receive the whole history first. In live-only mode
	// maxReplayed MUST stay 0 (ignore any Last-Event-ID/?from) so every live
	// frame passes the dedup — else live frames ≤ the cursor are wrongly skipped.
	maxReplayed := 0
	if r.URL.Query().Get("live") != "1" {
		maxReplayed = parseLastEventID(r)
		if events, lerr := a.host.ListEvents(r.Context(), id, store.ListEventsOpts{MinSeq: maxReplayed}); lerr == nil {
			for _, ev := range events {
				frame, ferr := store.EventRowToFrame(ev)
				if ferr != nil {
					continue
				}
				if writeErr := writeSSEFrame(w, rc, a.logger, ev.Seq, frame); writeErr != nil {
					return
				}
				if ev.Seq > maxReplayed {
					maxReplayed = ev.Seq
				}
			}
			flusher.Flush()
		} else {
			a.logger.Warn("httpapi: stream replay skipped", "id", id, "err", lerr)
		}
	}

	// Live.
	hb := time.NewTicker(sseHeartbeat)
	defer hb.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case f, open := <-sub:
			if !open {
				return
			}
			if alreadyReplayed(f.Seq(), maxReplayed) {
				continue // persisted frame already in the replay
			}
			if err := writeSSEFrame(w, rc, a.logger, f.Seq(), f); err != nil {
				return
			}
			if f.Seq() > maxReplayed {
				maxReplayed = f.Seq()
			}
			flusher.Flush()
		case <-hb.C:
			_ = rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// alreadyReplayed reports whether a live frame's seq was already delivered in
// the replay. Only persisted frames (seq > 0) can overlap; streaming chunks
// (seq 0, never persisted) always pass.
func alreadyReplayed(seq, maxReplayed int) bool {
	return seq > 0 && seq <= maxReplayed
}

// parseLastEventID reads the resume cursor from the Last-Event-ID header (SSE
// reconnect) or a ?from= query fallback. 0 (or invalid) ⇒ from the start.
func parseLastEventID(r *http.Request) int {
	v := r.Header.Get("Last-Event-ID")
	if v == "" {
		v = r.URL.Query().Get("from")
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// writeSSEFrame writes one frame as an SSE event: `id: <seq>` (persisted frames
// only) + `data: <wire-json>`. json.Marshal never emits raw newlines, so the
// payload is a single data line. Bounded by a write deadline (slow-client guard).
func writeSSEFrame(w io.Writer, rc *http.ResponseController, logger *slog.Logger, seq int, f protocol.Frame) error {
	data, err := sseCodec.EncodeFrame(f)
	if err != nil {
		// Skip an unencodable frame rather than kill the stream — but WARN, not
		// silently: a swallowed encode error here once hid the whole live view
		// (liveview frames built with an empty author failed Validate).
		if logger != nil {
			logger.Warn("httpapi: skipping unencodable SSE frame", "kind", f.Kind(), "err", err)
		}
		return nil
	}
	var b strings.Builder
	if seq > 0 {
		fmt.Fprintf(&b, "id: %d\n", seq)
	}
	b.WriteString("data: ")
	b.Write(data)
	b.WriteString("\n\n")
	_ = rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
	_, werr := io.WriteString(w, b.String())
	return werr
}
