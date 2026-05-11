package notepad

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/internal/fixture"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

// errStore returns a fixed error from every method. Used to drive
// the AppendNote-error-propagation path; the rest of the core
// tests run against the shared [fixture.TestStore].
type errStore struct{ err error }

func (e errStore) AppendNote(_ context.Context, _ store.NoteRow) error { return e.err }
func (e errStore) ListNotes(_ context.Context, _ string, _ store.ListNotesOpts) ([]store.NoteRow, error) {
	return nil, e.err
}
func (e errStore) SearchNotes(_ context.Context, _, _ string, _ store.ListNotesOpts) ([]store.NoteRow, error) {
	return nil, e.err
}

func TestAppend_PersistsRow(t *testing.T) {
	st := fixture.NewTestStore()
	np := New(st, "agent-1", "sess-1")
	id, err := np.Append(context.Background(), "user-9", "hello")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if id == "" {
		t.Fatal("empty id")
	}
	if len(st.Notes) != 1 {
		t.Fatalf("expected 1 row, got %d", len(st.Notes))
	}
	row := st.Notes[0]
	if row.ID != id || row.AgentID != "agent-1" || row.SessionID != "sess-1" || row.Content != "hello" {
		t.Errorf("row mismatch: %+v", row)
	}
	if row.CreatedAt.IsZero() {
		t.Error("CreatedAt not set")
	}
}

func TestAppend_RejectsEmptyAndOversize(t *testing.T) {
	np := New(fixture.NewTestStore(), "agent-1", "sess-1")
	if _, err := np.Append(context.Background(), "u", ""); err == nil {
		t.Error("empty text accepted")
	}
	if _, err := np.Append(context.Background(), "u", strings.Repeat("a", noteMaxBytes+1)); err == nil {
		t.Error("oversize text accepted")
	}
}

func TestAppend_PropagatesStoreError(t *testing.T) {
	want := errors.New("boom")
	np := New(errStore{err: want}, "agent-1", "sess-1")
	_, err := np.Append(context.Background(), "u", "ok")
	if !errors.Is(err, want) {
		t.Fatalf("expected wrapped store err, got %v", err)
	}
}

func TestList_ReturnsRowsForSession(t *testing.T) {
	st := fixture.NewTestStore()
	st.Notes = []store.NoteRow{
		{ID: "n1", SessionID: "sess-1", AuthorSessionID: "sess-1", Content: "a", CreatedAt: time.Now()},
		{ID: "n2", SessionID: "sess-2", AuthorSessionID: "sess-2", Content: "b", CreatedAt: time.Now()},
		{ID: "n3", SessionID: "sess-1", AuthorSessionID: "sess-1", Content: "c", CreatedAt: time.Now()},
	}
	np := New(st, "agent-1", "sess-1")
	notes, err := np.List(context.Background(), 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(notes) != 2 {
		t.Fatalf("expected 2 notes for sess-1, got %d", len(notes))
	}
	// Phase 4.2.3: ListNotes returns DESC by created_at; newest
	// (n3 / "c") first, oldest second.
	if notes[0].Text != "c" || notes[1].Text != "a" {
		t.Errorf("unexpected texts: %v %v", notes[0].Text, notes[1].Text)
	}
}
