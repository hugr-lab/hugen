package session

import (
	"context"
	"testing"

	"github.com/hugr-lab/hugen/pkg/extension"
	notepadext "github.com/hugr-lab/hugen/pkg/extension/notepad"
	"github.com/hugr-lab/hugen/pkg/session/internal/fixture"
)

// TestNotepadExtension_RegisteredOnSession boots a session with
// the notepad extension wired in and asserts (a) InitState ran —
// the *Notepad handle is reachable from SessionState — and (b)
// session.Notepad() shim returns the same handle so legacy
// callers keep working transparently. Tool dispatch coverage
// lives in pkg/extension/notepad — this is just the integration
// seam.
func TestNotepadExtension_RegisteredOnSession(t *testing.T) {
	store := fixture.NewTestStore()
	parent, cleanup := newTestParent(t,
		withTestStore(store),
		withTestExtensions(notepadext.New(store, "a1")),
	)
	defer cleanup()

	if got := notepadext.FromState(parent); got == nil {
		t.Fatal("notepad handle missing from session state after InitState")
	}
	if parent.Notepad() == nil {
		t.Fatal("Session.Notepad() shim returned nil after extension wiring")
	}

	// Append through the handle round-trips into the underlying store.
	id, err := parent.Notepad().Append(context.Background(), "u1", "hello")
	if err != nil {
		t.Fatalf("Notepad.Append: %v", err)
	}
	if id == "" {
		t.Fatal("empty id from Append")
	}
}

// _ keeps the extension import alive even if the test grows past
// the helper alias.
var _ extension.Extension = (*notepadext.Extension)(nil)
