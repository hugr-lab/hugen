package notepad

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeStore struct {
	rows []NoteRow
	err  error
}

func (f *fakeStore) AppendNote(_ context.Context, row NoteRow) error {
	if f.err != nil {
		return f.err
	}
	f.rows = append(f.rows, row)
	return nil
}

func (f *fakeStore) ListNotes(_ context.Context, sessionID string, limit int) ([]NoteRow, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]NoteRow, 0, len(f.rows))
	for _, r := range f.rows {
		if r.SessionID != sessionID {
			continue
		}
		out = append(out, r)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func TestAppend_PersistsRow(t *testing.T) {
	store := &fakeStore{}
	np := New(store, "agent-1", "sess-1")
	id, err := np.Append(context.Background(), "user-9", "hello")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if id == "" {
		t.Fatal("empty id")
	}
	if len(store.rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(store.rows))
	}
	row := store.rows[0]
	if row.ID != id || row.AgentID != "agent-1" || row.SessionID != "sess-1" || row.Content != "hello" {
		t.Errorf("row mismatch: %+v", row)
	}
	if row.CreatedAt.IsZero() {
		t.Error("CreatedAt not set")
	}
}

func TestAppend_RejectsEmptyAndOversize(t *testing.T) {
	np := New(&fakeStore{}, "agent-1", "sess-1")
	if _, err := np.Append(context.Background(), "u", ""); err == nil {
		t.Error("empty text accepted")
	}
	if _, err := np.Append(context.Background(), "u", strings.Repeat("a", noteMaxBytes+1)); err == nil {
		t.Error("oversize text accepted")
	}
}

func TestAppend_PropagatesStoreError(t *testing.T) {
	want := errors.New("boom")
	np := New(&fakeStore{err: want}, "agent-1", "sess-1")
	_, err := np.Append(context.Background(), "u", "ok")
	if !errors.Is(err, want) {
		t.Fatalf("expected wrapped store err, got %v", err)
	}
}

func TestList_ReturnsRowsForSession(t *testing.T) {
	store := &fakeStore{rows: []NoteRow{
		{ID: "n1", SessionID: "sess-1", AuthorSessionID: "sess-1", Content: "a", CreatedAt: time.Now()},
		{ID: "n2", SessionID: "sess-2", AuthorSessionID: "sess-2", Content: "b", CreatedAt: time.Now()},
		{ID: "n3", SessionID: "sess-1", AuthorSessionID: "sess-1", Content: "c", CreatedAt: time.Now()},
	}}
	np := New(store, "agent-1", "sess-1")
	notes, err := np.List(context.Background(), 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(notes) != 2 {
		t.Fatalf("expected 2 notes for sess-1, got %d", len(notes))
	}
	if notes[0].Text != "a" || notes[1].Text != "c" {
		t.Errorf("unexpected texts: %v %v", notes[0].Text, notes[1].Text)
	}
}
