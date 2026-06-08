// Package recap is the live-topic primitive: a short, embedding-sized
// marker of what a session is discussing right now, for db-2's dynamic-
// skill recall and Phase 7's offline distiller. It runs for EVERY session:
// a root distils a rolling marker from the conversation; a subagent distils
// its marker ONCE from its delegated task (its goal is fixed).
//
// The marker is (re)formed by a cheap model call at the turn boundary,
// SYNCHRONOUSLY, before the turn renders — so the skill advertise reads a
// current topic. There is NO accumulating fold to store: the handle keeps
// only a small bounded ring of recent messages plus the latest marker.
// The model is given "what the dialogue is about" (the prior marker), the
// recent completed exchanges, and the new user message(s), and returns an
// updated marker. The marker is emitted as a CategoryOp ExtensionFrame so a
// restart replays it (the ring is rebuilt from the tail of history).
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

// Recap is the marker a consumer reads:
//
//   - Topic — a 3-5 word headline naming the conversation, for display.
//   - Text — the substantive theme (one or two sentences): the text db-2
//     embeds as the recall anchor.
//   - Categories — keywords for display + Phase 7's memory index.
type Recap struct {
	Topic      string   `json:"topic,omitempty"`
	Text       string   `json:"text"`
	Categories []string `json:"categories,omitempty"`
}

// message is one ring entry: a dialogue turn's role + (truncated) text.
type message struct {
	Role string
	Text string
}

// sessionRecap is the per-session handle: a bounded ring of recent
// messages + the latest marker.
type sessionRecap struct {
	mu sync.Mutex

	// root marks a depth-0 session. Root re-forms the marker every turn
	// (rolling conversation); a subagent forms it ONCE at start from its
	// task (the goal is fixed), so its fold is gated on having no marker.
	root bool
	// recent is a bounded ring of the latest dialogue messages, oldest
	// first. Trailing "user" entries (no assistant reply yet) are the
	// turn's NEW messages; the rest is recent context.
	recent []message
	// marker is the latest formed topic. Empty until the first fold.
	marker Recap

	// inflight guards against concurrent folds.
	inflight bool
}

// hasMarker reports whether a marker has been formed yet.
func (s *sessionRecap) hasMarker() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.marker.Text != ""
}

// appendMessage records a dialogue message into the ring, truncating it to
// maxMsgChars (so one long turn can't dominate the marker) and evicting the
// oldest entries beyond maxRing.
func (s *sessionRecap) appendMessage(role, text string, maxMsgChars, maxRing int) {
	if maxMsgChars > 0 && len(text) > maxMsgChars {
		text = text[:maxMsgChars]
	}
	if text == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recent = append(s.recent, message{Role: role, Text: text})
	if maxRing > 0 && len(s.recent) > maxRing {
		s.recent = append(s.recent[:0], s.recent[len(s.recent)-maxRing:]...)
	}
}

// snapshotForFold splits the ring into the fold inputs: the prior marker,
// the recent completed exchanges (capped to recentContext entries), and the
// turn's NEW user messages (the trailing run of "user" entries with no
// assistant reply yet — there may be several, e.g. a spawn's goal+inputs).
// Returns copies, safe to use outside the lock.
func (s *sessionRecap) snapshotForFold(recentContext int) (prior Recap, recent []message, fresh []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prior = s.marker
	n := len(s.recent)
	// Trailing user messages = the turn's new input.
	i := n
	for i > 0 && s.recent[i-1].Role == "user" {
		i--
	}
	for _, m := range s.recent[i:n] {
		fresh = append(fresh, m.Text)
	}
	// Recent context = the messages before, last recentContext entries.
	start := max(0, i-recentContext)
	recent = append(recent, s.recent[start:i]...)
	return prior, recent, fresh
}

// setMarker installs a freshly formed marker.
func (s *sessionRecap) setMarker(rec Recap) {
	s.mu.Lock()
	s.marker = rec
	s.mu.Unlock()
}

// current returns the marker for USE and whether one exists. Falls back to
// the raw recent ring when no marker has been formed yet (or the fold
// failed) so the topic is never empty while there is dialogue — db-2 always
// has an anchor.
func (s *sessionRecap) current() (Recap, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.marker.Text != "" {
		return s.marker, true
	}
	if len(s.recent) == 0 {
		return Recap{}, false
	}
	var b strings.Builder
	for _, m := range s.recent {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(m.Role)
		b.WriteString(": ")
		b.WriteString(m.Text)
	}
	return Recap{Text: b.String()}, true
}

// beginRefresh marks a fold in-flight, returning false when one is already
// running (single-flight guard).
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

// restore seeds the latest marker from a replayed frame. The ring is
// rebuilt separately (Recover re-adds the tail of history), and the next
// turn boundary re-forms the marker regardless.
func (s *sessionRecap) restore(rec Recap) {
	s.mu.Lock()
	s.marker = rec
	s.mu.Unlock()
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

// CurrentRecap is the public read accessor db-2 (and other consumers) use:
// the current topic marker for the root session reachable from state, or
// (zero, false) for a non-root session or one with no dialogue yet. Never
// blocks.
func CurrentRecap(state extension.SessionState) (Recap, bool) {
	h := FromState(state)
	if h == nil {
		return Recap{}, false
	}
	return h.current()
}
