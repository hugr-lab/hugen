// Package whiteboard is the pure projection layer for phase-4 §7.
// A whiteboard is a parent-mediated broadcast channel: the host
// owns the canonical list of messages, members each persist their
// own copy on receipt of a broadcast Frame.
//
// This package is deliberately independent of pkg/session and
// pkg/protocol — it walks a flat ProjectEvent slice and emits a
// Whiteboard projection. The session package converts EventRow /
// WhiteboardOpPayload into ProjectEvent at materialise / apply
// time so cycles stay outside the projection layer.
//
// Caps (per phase-4-spec §7.3):
//
//   - per-message text capped at MaxMessageBytes (with a truncation
//     marker); a Truncated flag flows through the event.
//   - projection retains at most MaxMessages OR MaxTotalBytes,
//     whichever is tighter; FIFO eviction on append. Eviction is a
//     projection concern only — events stay in the log forever.
package whiteboard

import "time"

const (
	// MaxMessageBytes caps a single message's text. Anything longer
	// is truncated with TruncationMarker appended; the event records
	// Truncated=true so audits can spot the cut.
	MaxMessageBytes = 4096

	// MaxMessages caps the in-memory message ring. FIFO eviction.
	MaxMessages = 100

	// MaxTotalBytes caps the cumulative byte size of retained
	// messages. FIFO eviction whichever cap fires first.
	MaxTotalBytes = 32 * 1024

	// TruncationMarker is appended to a message text that exceeded
	// MaxMessageBytes — visible to the reader so a downstream model
	// knows the broadcast was clipped.
	TruncationMarker = "\n[…truncated]"
)

// Op constants mirror the protocol.WhiteboardOpPayload.Op values.
// Mirrored here so the projection layer doesn't import pkg/protocol.
const (
	OpInit  = "init"
	OpWrite = "write"
	OpStop  = "stop"
)

// Whiteboard is the in-memory projection a session keeps for either
// the board it hosts or the board it is a member of. NextSeq is the
// host-monotonic counter used when the host stamps the next outgoing
// write; member-side projections inherit the seq from each broadcast
// they observe (NextSeq stays at zero on member-only sides).
type Whiteboard struct {
	Active    bool
	StartedAt time.Time
	StoppedAt time.Time
	NextSeq   int64
	Messages  []Message
	totalBytes int
}

// Message is one retained broadcast — Newest at the end of
// Whiteboard.Messages. Truncated means the text was clipped at
// MaxMessageBytes when the broadcast was originally written.
type Message struct {
	Seq           int64
	At            time.Time
	FromSessionID string
	FromRole      string
	Text          string
	Truncated     bool
}

// ProjectEvent is the wire-shape the session package converts to
// before calling Project / Apply. Op is one of OpInit / OpWrite /
// OpStop. For OpWrite, Seq and FromSessionID + FromRole + Text +
// Truncated populate the resulting Message.
type ProjectEvent struct {
	At            time.Time
	Op            string
	Seq           int64
	FromSessionID string
	FromRole      string
	Text          string
	Truncated     bool
}

// Project replays an ordered slice of events into a Whiteboard. The
// latest OpInit / OpStop boundary defines Active and StartedAt;
// OpWrite events appended after the latest OpInit append messages
// (subject to eviction caps).
func Project(events []ProjectEvent) Whiteboard {
	var wb Whiteboard
	for _, ev := range events {
		wb = Apply(wb, ev)
	}
	return wb
}

// Apply is the pure incremental step. Re-applying the events Project
// already saw is a no-op: callers pass each event exactly once.
func Apply(wb Whiteboard, ev ProjectEvent) Whiteboard {
	switch ev.Op {
	case OpInit:
		wb.Active = true
		wb.StartedAt = ev.At
		wb.StoppedAt = time.Time{}
		wb.Messages = nil
		wb.totalBytes = 0
		if wb.NextSeq == 0 {
			wb.NextSeq = 1
		}
	case OpStop:
		wb.Active = false
		wb.StoppedAt = ev.At
	case OpWrite:
		if !wb.Active {
			// Defensive: writes that arrive before init or after stop
			// are dropped from the projection. Events still live in
			// the log for audit.
			return wb
		}
		text, truncated := truncate(ev.Text)
		if ev.Truncated {
			truncated = true
		}
		msg := Message{
			Seq:           ev.Seq,
			At:            ev.At,
			FromSessionID: ev.FromSessionID,
			FromRole:      ev.FromRole,
			Text:          text,
			Truncated:     truncated,
		}
		wb.Messages = append(wb.Messages, msg)
		wb.totalBytes += len(text)
		if ev.Seq >= wb.NextSeq {
			wb.NextSeq = ev.Seq + 1
		}
		evictExcess(&wb)
	}
	return wb
}

// truncate clips text to MaxMessageBytes, appending TruncationMarker
// when a cut happens. Returns the (possibly clipped) text and a
// truncated flag.
func truncate(text string) (string, bool) {
	if len(text) <= MaxMessageBytes {
		return text, false
	}
	cut := MaxMessageBytes - len(TruncationMarker)
	if cut < 0 {
		cut = 0
	}
	return text[:cut] + TruncationMarker, true
}

// evictExcess removes oldest messages until both caps hold.
func evictExcess(wb *Whiteboard) {
	for len(wb.Messages) > MaxMessages {
		wb.totalBytes -= len(wb.Messages[0].Text)
		wb.Messages = wb.Messages[1:]
	}
	for wb.totalBytes > MaxTotalBytes && len(wb.Messages) > 1 {
		wb.totalBytes -= len(wb.Messages[0].Text)
		wb.Messages = wb.Messages[1:]
	}
	if wb.totalBytes < 0 {
		wb.totalBytes = 0
	}
}
