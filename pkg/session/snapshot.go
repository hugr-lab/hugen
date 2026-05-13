package session

import (
	"context"
	"fmt"
	"time"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

// SessionSnapshot is the point-in-time projection
// [Manager.SnapshotSession] returns. Attaching adapters (TUI
// reconnect, SSE late-join, future A2A peer join) render this
// instead of replaying every event from the store. Phase 5.1b §3.
//
// Every field is a value or a copy — no shared references with
// the live session escape, so the adapter is free to mutate the
// returned struct without affecting the session.
type SessionSnapshot struct {
	SessionID string `json:"session_id"`
	State     string `json:"state"`
	Depth     int    `json:"depth"`
	// Skill / Role are the spawn metadata for sub-agents. Empty
	// for a root session.
	Skill string `json:"skill,omitempty"`
	Role  string `json:"role,omitempty"`

	OpenedAt time.Time `json:"opened_at"`
	// LastTurn is the timestamp of the most recent
	// SessionStatusActive transition observed on this session,
	// or the OpenedAt fallback when no turn has run yet.
	LastTurn time.Time `json:"last_turn,omitempty"`
	// TurnsUsed is the current within-turn iteration count
	// (model→tools→model loop index). 0 when idle. Adapters
	// render contextually.
	TurnsUsed int `json:"turns_used,omitempty"`

	LoadedSkills []string `json:"loaded_skills,omitempty"`

	ActiveSubagents []protocol.ActiveSubagentRef `json:"active_subagents,omitempty"`
	PendingInquiry  *protocol.PendingInquiryRef  `json:"pending_inquiry,omitempty"`
	LastToolCall    *protocol.ToolCallRef        `json:"last_tool_call,omitempty"`

	Plan       extension.PlanSnapshotData `json:"plan,omitempty"`
	Notepad    []extension.NoteRef        `json:"notepad,omitempty"`
	Whiteboard string                     `json:"whiteboard,omitempty"`

	// EventsTail is the most recent N events in this session's
	// log, ordered oldest-first. Populated only when the caller
	// asks for it via SnapshotOptions.IncludeEventsTail — adapters
	// that only need the state header skip the store round-trip.
	EventsTail []store.EventRow `json:"events_tail,omitempty"`
}

// SnapshotOptions controls what Session.Snapshot includes. Zero
// value returns the cheap path (no EventsTail).
type SnapshotOptions struct {
	// IncludeEventsTail caps the number of recent events to fetch
	// from the store. 0 means "skip events entirely"; -1 means
	// "use the package default (100)".
	IncludeEventsTail int
}

// ErrSnapshotSessionNotFound is returned by
// [Manager.SnapshotSession] when the requested session id is not
// in the live registry or any of its descendant trees.
var ErrSnapshotSessionNotFound = fmt.Errorf("session: snapshot: session not found")

// Snapshot returns a point-in-time projection of the session's
// state. Safe to call from any goroutine — reads under each
// piece of state's existing mutex.
//
// The default (zero SnapshotOptions) skips the events-tail
// store read. Pass `SnapshotOptions{IncludeEventsTail: -1}` to
// fetch the default cap (100), or any positive integer for a
// custom cap.
func (s *Session) Snapshot(ctx context.Context, opts SnapshotOptions) (*SessionSnapshot, error) {
	if s == nil {
		return nil, ErrSnapshotSessionNotFound
	}
	out := &SessionSnapshot{
		SessionID: s.id,
		State:     s.Status(),
		Depth:     s.depth,
		Skill:     s.spawnSkill,
		Role:      s.spawnRole,
		OpenedAt:  s.openedAt,
	}
	// LastTurn proxy: walk back from the current Run loop's
	// turnState if active; else fall back to OpenedAt.
	if st := s.turnState; st != nil {
		out.TurnsUsed = st.iter
		out.LastTurn = time.Now().UTC()
	} else {
		out.LastTurn = s.openedAt
	}

	// Inquiry / subagents / last tool call — reuse the same
	// helpers populateStatusSnapshot uses so the two surfaces
	// stay consistent.
	out.ActiveSubagents = s.snapshotActiveSubagents()
	out.PendingInquiry = s.snapshotPendingInquiry()
	if last := s.lastToolCall.Load(); last != nil {
		clone := *last
		out.LastToolCall = &clone
	}

	// Extension contributions. Each capability is optional;
	// extensions that don't implement it are silently skipped.
	if s.deps != nil {
		for _, ext := range s.deps.Extensions {
			if c, ok := ext.(extension.LoadedSkillsContributor); ok {
				out.LoadedSkills = append(out.LoadedSkills, c.LoadedSkillNames(ctx, s)...)
			}
			if c, ok := ext.(extension.PlanContributor); ok {
				if d := c.PlanSnapshot(ctx, s); d.Body != "" || d.CurrentStep != "" {
					out.Plan = d
				}
			}
			if c, ok := ext.(extension.NotesContributor); ok {
				limit := opts.IncludeEventsTail // re-use the cap as a sane proxy
				if limit <= 0 {
					limit = 10
				}
				notes := c.NotesSnapshot(ctx, s, limit)
				if len(notes) > 0 {
					out.Notepad = append(out.Notepad, notes...)
				}
			}
			if c, ok := ext.(extension.WhiteboardContributor); ok {
				if wb := c.WhiteboardSnapshot(ctx, s); wb != "" {
					out.Whiteboard = wb
				}
			}
		}
	}

	// EventsTail is opt-in. Today the store API does not have a
	// tail-N primitive (ListEventsOpts has MinSeq + Limit, both
	// oldest-first). Implementing tail-N efficiently needs either
	// a new store method or a MaxSeq+Order=desc extension —
	// scheduled as a follow-up to keep β scope contained. When
	// the store grows the API, fill EventsTail here and drop this
	// no-op branch.
	_ = opts.IncludeEventsTail
	return out, nil
}

// FindDescendant walks the session tree rooted at s and returns
// the descendant with the given id, or nil when no descendant
// matches. Used by [Manager.SnapshotSession] to look up sub-
// agents (Manager.live only tracks root sessions, but adapters
// may want a snapshot of a worker / mission too).
//
// Concurrency: holds childMu only briefly while inspecting each
// level — returns a *Session that may be torn down concurrently;
// callers must handle that race (e.g. by treating a closed
// session's Snapshot as "session went away").
func (s *Session) FindDescendant(id string) *Session {
	if s == nil {
		return nil
	}
	if s.id == id {
		return s
	}
	s.childMu.Lock()
	children := make([]*Session, 0, len(s.children))
	for _, c := range s.children {
		if c != nil {
			children = append(children, c)
		}
	}
	s.childMu.Unlock()
	for _, c := range children {
		if got := c.FindDescendant(id); got != nil {
			return got
		}
	}
	return nil
}
