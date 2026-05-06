package session

import (
	"github.com/hugr-lab/hugen/pkg/session/notepad"
)

// Notepad / Note / NoteRow are re-exports of the notepad subpackage
// types so external callers (cmd/hugen, adapters, tests) compile
// unchanged after the phase-4.1a step-17 split.
type (
	Notepad = notepad.Notepad
	Note    = notepad.Note
	NoteRow = notepad.NoteRow
)

// NewNotepad constructs a Notepad bound to one session. Thin
// shim over notepad.New to keep the legacy package-level call site
// working.
func NewNotepad(store RuntimeStore, agentID, sessionID string) *Notepad {
	return notepad.New(store, agentID, sessionID)
}
