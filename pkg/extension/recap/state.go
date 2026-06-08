// Package recap is the live-topic primitive: a compact, embedding-sized
// descriptor of what a root session is discussing right now, for db-2's
// dynamic-skill recall and Phase 7's offline distiller. Root sessions
// only; subagents carry the task/wave brief as their topic.
//
// The descriptor is built WITHOUT blocking the conversation. State holds
// two parts:
//
//   - recap — the compressed topic folded up to a watermark seq, bounded
//     to ~MaxRecapTokens so it stays embeddable. Compressed by a cheap
//     async model call, and ONLY when it needs to be.
//   - tail — the raw user↔assistant messages AFTER the watermark, not yet
//     folded in.
//
// What a consumer reads is the EFFECTIVE topic = recap ⊕ tail, always
// available with zero wait: the first user message is the initial topic
// (recap empty, tail = [that message]); every later message just appends
// to the tail. Only when recap ⊕ tail grows past a fraction of
// MaxRecapTokens does an async summarizer fold the tail into recap and
// advance the watermark — so most turns cost no model call, and the next
// message never waits on a summarization. The compressed recap +
// watermark is emitted as a CategoryOp ExtensionFrame so a restart
// replays it (the tail is rebuilt from history past the watermark).
package recap

import (
	"strings"
	"sync"

	"github.com/hugr-lab/hugen/pkg/extension"
)

// StateKey is the [extension.SessionState] key the per-session handle is
// stored under.
const StateKey = "recap"

// providerName is the stable extension name stamped on emitted
// ExtensionFrames (Recover filters on it).
const providerName = "recap"

// charsPerToken is the char/4 heuristic the runtime uses everywhere in
// place of a per-model tokeniser. Recap sizes are configured in tokens
// and converted to chars through this.
const charsPerToken = 4

// Recap is the descriptor a consumer reads. Two outputs:
//
//   - Topic — a 3-5 word headline naming the conversation, for display +
//     a coarse change signal. Empty until the first fold.
//   - Text — the substantive rolling recap. As read (effective), it is
//     the compressed long recap ⊕ the raw un-folded tail — the rich
//     query db-2 embeds for skill/memory recall.
//
// Categories are keywords from the last fold; ChangeConfidence is the
// embedding distance between the previous and current compressed LONG
// recap, set at fold time (0 before the first fold / when no embedder).
type Recap struct {
	Topic            string   `json:"topic,omitempty"`
	Text             string   `json:"text"`
	Categories       []string `json:"categories,omitempty"`
	ChangeConfidence float64  `json:"change_confidence,omitempty"`
}

// tailTurn is one un-folded dialogue message: its seq (for the watermark)
// plus role + text.
type tailTurn struct {
	Seq  int64
	Role string
	Text string
}

// sessionRecap is the per-root-session handle.
type sessionRecap struct {
	mu sync.Mutex

	// compressed is the folded topic up to watermarkSeq (Text +
	// Categories + ChangeConfidence). Empty until the first fold.
	compressed Recap
	// watermarkSeq is the highest message seq folded into compressed.
	watermarkSeq int64
	// tail holds raw messages with Seq > watermarkSeq, oldest-first.
	tail      []tailTurn
	tailChars int

	// inflight guards against concurrent summarizers.
	inflight bool
}

// appendTurn records a dialogue message into the tail when it post-dates
// the watermark (already-folded messages are dropped). Each message is
// truncated to maxMsgChars first so one long turn can't dominate the
// tail (and so the first user message — the initial topic — stays
// embeddable). windowCap bounds the whole tail as a safety valve if
// summarization is stuck: the oldest tail turns evict so the in-memory
// window can't grow without bound.
func (s *sessionRecap) appendTurn(seq int64, role, text string, maxMsgChars, windowCap int) {
	if maxMsgChars > 0 && len(text) > maxMsgChars {
		text = text[:maxMsgChars]
	}
	if text == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if seq <= s.watermarkSeq {
		return
	}
	s.tail = append(s.tail, tailTurn{Seq: seq, Role: role, Text: text})
	s.tailChars += len(text)
	for s.tailChars > windowCap && len(s.tail) > 1 {
		s.tailChars -= len(s.tail[0].Text)
		s.tail = s.tail[1:]
	}
}

// effective returns the topic for USE — the compressed recap text plus
// the raw un-folded tail — and whether anything exists yet. Lock-safe;
// the read path db-2's advertise uses. Zero wait: this never blocks on a
// summarization.
func (s *sessionRecap) effective() (Recap, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.compressed.Text == "" && len(s.tail) == 0 {
		return Recap{}, false
	}
	var b strings.Builder
	b.WriteString(s.compressed.Text)
	for _, t := range s.tail {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(t.Role)
		b.WriteString(": ")
		b.WriteString(t.Text)
	}
	return Recap{
		Topic:            s.compressed.Topic,
		Text:             b.String(),
		Categories:       s.compressed.Categories,
		ChangeConfidence: s.compressed.ChangeConfidence,
	}, true
}

// needsFold reports whether recap ⊕ tail has grown past thresholdChars
// (the configured fraction of the recap byte budget) — the trigger to
// fold the tail into the compressed recap.
func (s *sessionRecap) needsFold(thresholdChars int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.compressed.Text)+s.tailChars > thresholdChars
}

// beginRefresh marks a summarizer in-flight, returning false when one is
// already running.
func (s *sessionRecap) beginRefresh() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight {
		return false
	}
	s.inflight = true
	return true
}

func (s *sessionRecap) endRefresh() {
	s.mu.Lock()
	s.inflight = false
	s.mu.Unlock()
}

// snapshotForFold returns the inputs the async summarizer folds: the
// prior compressed text, a copy of the current tail, and the highest
// tail seq (the new watermark). Empty turns slice → nothing to fold.
func (s *sessionRecap) snapshotForFold() (prevText string, turns []tailTurn, hiSeq int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prevText = s.compressed.Text
	turns = append([]tailTurn(nil), s.tail...)
	for _, t := range s.tail {
		if t.Seq > hiSeq {
			hiSeq = t.Seq
		}
	}
	return prevText, turns, hiSeq
}

// commitFold installs the freshly summarized recap, advances the
// watermark to hiSeq, and drops tail turns at/under hiSeq (now folded).
// Turns that arrived DURING summarization (Seq > hiSeq) survive in the
// tail, so nothing is lost.
func (s *sessionRecap) commitFold(rec Recap, hiSeq int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.compressed = rec
	if hiSeq > s.watermarkSeq {
		s.watermarkSeq = hiSeq
	}
	kept := s.tail[:0]
	chars := 0
	for _, t := range s.tail {
		if t.Seq > s.watermarkSeq {
			kept = append(kept, t)
			chars += len(t.Text)
		}
	}
	s.tail = kept
	s.tailChars = chars
}

// restore seeds the compressed recap + watermark from a replayed frame,
// clearing the tail (Recover re-adds post-watermark messages from
// history afterwards).
func (s *sessionRecap) restore(rec Recap, watermark int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.compressed = rec
	s.watermarkSeq = watermark
	s.tail = nil
	s.tailChars = 0
}

// FromState returns the *sessionRecap handle for state, or nil when
// InitState did not run for it (non-root sessions, or a session created
// without the recap extension).
func FromState(state extension.SessionState) *sessionRecap {
	v, ok := state.Value(StateKey)
	if !ok {
		return nil
	}
	h, _ := v.(*sessionRecap)
	return h
}

// CurrentRecap is the public read accessor db-2 (and other consumers)
// use: the EFFECTIVE topic (compressed recap ⊕ un-folded tail) for the
// root session reachable from state, or (zero, false) for a non-root
// session or one with no dialogue yet. Never blocks on a summarization.
func CurrentRecap(state extension.SessionState) (Recap, bool) {
	h := FromState(state)
	if h == nil {
		return Recap{}, false
	}
	return h.effective()
}
