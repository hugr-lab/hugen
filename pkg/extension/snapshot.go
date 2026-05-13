package extension

import (
	"context"
	"time"
)

// Phase 5.1b: per-extension contributor capabilities for the
// Manager.SnapshotSession point-in-time projection.
//
// Each capability is narrow on purpose — an extension implements
// only the contribution it owns and Session.Snapshot type-asserts
// each interface in turn. No central registry, no
// fields-by-extension-name plumbing. Adding a new contribution
// type later is a backward-compatible addition: declare a new
// interface here, add the type-assert in pkg/session/snapshot.go,
// extensions that don't implement it stay quietly inert.
//
// The interfaces are intentionally read-only — they MUST NOT
// mutate session state. Returned slices are owned by the caller
// (Session.Snapshot copies them onward to *session.SessionSnapshot
// fields); contributor implementations should return defensive
// copies of any backing data they hold under their own mutexes.

// LoadedSkillsContributor surfaces the names of skills bound to
// the calling session. Implemented by the skill extension.
type LoadedSkillsContributor interface {
	LoadedSkillNames(ctx context.Context, state SessionState) []string
}

// PlanContributor surfaces the current plan body (markdown text)
// + the index of the active step. Implemented by the plan
// extension. Returns an empty string when no plan is set.
type PlanContributor interface {
	PlanSnapshot(ctx context.Context, state SessionState) PlanSnapshotData
}

// PlanSnapshotData carries plan extension state for the
// SessionSnapshot. Fields stay omitempty in JSON. CurrentStep is
// the human-readable step label as the plan extension stores it
// (e.g. "Explore", "Analyze") — adapters render it verbatim;
// the numeric "step N of M" indicator is the progress
// extension's job, not the snapshot's.
type PlanSnapshotData struct {
	Body        string `json:"body,omitempty"`
	CurrentStep string `json:"current_step,omitempty"`
}

// NotesContributor surfaces recent notepad entries for the
// calling session. Implemented by the notepad extension. limit
// caps the returned slice; 0 means "use the default" (10).
type NotesContributor interface {
	NotesSnapshot(ctx context.Context, state SessionState, limit int) []NoteRef
}

// NoteRef is the projected shape of a notepad note for adapter
// rendering. Lean — not every column of session_notes; just
// what a status panel needs.
type NoteRef struct {
	Category  string    `json:"category"`
	Mission   string    `json:"mission,omitempty"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

// WhiteboardContributor surfaces the current whiteboard text for
// the calling session. Implemented by the whiteboard extension.
// Returns an empty string when no whiteboard is active.
type WhiteboardContributor interface {
	WhiteboardSnapshot(ctx context.Context, state SessionState) string
}
