package notepad

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/internal/fixture"
	"github.com/hugr-lab/hugen/pkg/session/store"
	"github.com/hugr-lab/hugen/pkg/skill"
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

// newNotepad builds a *Notepad bound to root sess-1 with the
// default 48h window. Helper for the unit tests that don't need a
// SessionState fixture.
func newNotepad(s Store) *Notepad {
	return New(s, "agent-1", "sess-1", "sess-1", skill.TierRoot, 0)
}

func TestAppend_PersistsRowWithMetadata(t *testing.T) {
	st := fixture.NewTestStore()
	np := newNotepad(st)
	id, err := np.Append(context.Background(), AppendInput{
		Content:  "orders.deleted_at appears to mark soft-deletes",
		Category: "schema-finding",
		Mission:  "northwind domain overview",
	})
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
	if row.SessionID != "sess-1" || row.AuthorSessionID != "sess-1" {
		t.Errorf("rootID/author mismatch: %+v", row)
	}
	if row.Category != "schema-finding" {
		t.Errorf("category = %q, want schema-finding", row.Category)
	}
	if row.AuthorRole != skill.TierRoot {
		t.Errorf("author_role = %q, want %q", row.AuthorRole, skill.TierRoot)
	}
	if row.Mission != "northwind domain overview" {
		t.Errorf("mission = %q", row.Mission)
	}
	if row.Content != "orders.deleted_at appears to mark soft-deletes" {
		t.Errorf("content mismatch: %q", row.Content)
	}
}

func TestAppend_ClimbsToRoot(t *testing.T) {
	st := fixture.NewTestStore()
	// Worker session (depth 2) writing — rootID provided by
	// constructor mirrors the InitState walk.
	np := New(st, "agent-1", "ses-worker", "ses-root", skill.TierWorker, 0)
	if _, err := np.Append(context.Background(), AppendInput{Content: "x"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if len(st.Notes) != 1 {
		t.Fatalf("expected 1 row")
	}
	row := st.Notes[0]
	if row.SessionID != "ses-root" {
		t.Errorf("storage SessionID = %q, want ses-root (climb-to-root)", row.SessionID)
	}
	if row.AuthorSessionID != "ses-worker" {
		t.Errorf("AuthorSessionID = %q, want ses-worker", row.AuthorSessionID)
	}
	if row.AuthorRole != skill.TierWorker {
		t.Errorf("author_role = %q, want worker", row.AuthorRole)
	}
}

func TestAppend_EmptyContent(t *testing.T) {
	np := newNotepad(fixture.NewTestStore())
	if _, err := np.Append(context.Background(), AppendInput{Content: "   "}); err == nil {
		t.Fatal("expected error on blank content")
	}
}

func TestAppend_TooLong(t *testing.T) {
	np := newNotepad(fixture.NewTestStore())
	big := strings.Repeat("x", noteMaxBytes+1)
	if _, err := np.Append(context.Background(), AppendInput{Content: big}); err == nil {
		t.Fatal("expected error on oversize content")
	}
}

func TestAppend_StorePropagatesError(t *testing.T) {
	want := errors.New("boom")
	np := New(errStore{err: want}, "agent-1", "sess-1", "sess-1", skill.TierRoot, 0)
	_, err := np.Append(context.Background(), AppendInput{Content: "ok"})
	if !errors.Is(err, want) {
		t.Fatalf("expected wrapped store err, got %v", err)
	}
}

func TestRead_DESCByCreatedAt(t *testing.T) {
	st := fixture.NewTestStore()
	now := time.Now()
	st.Notes = []store.NoteRow{
		{ID: "n1", SessionID: "sess-1", AuthorSessionID: "sess-1", Content: "a", CreatedAt: now.Add(-3 * time.Hour)},
		{ID: "n2", SessionID: "sess-2", AuthorSessionID: "sess-2", Content: "b", CreatedAt: now.Add(-2 * time.Hour)},
		{ID: "n3", SessionID: "sess-1", AuthorSessionID: "sess-1", Content: "c", CreatedAt: now.Add(-1 * time.Hour)},
	}
	np := newNotepad(st)
	notes, err := np.Read(context.Background(), ReadInput{})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(notes) != 2 {
		t.Fatalf("expected 2 notes for sess-1, got %d", len(notes))
	}
	if notes[0].Text != "c" || notes[1].Text != "a" {
		t.Errorf("expected DESC order [c, a]; got %s / %s", notes[0].Text, notes[1].Text)
	}
}

func TestRead_CategoryFilter(t *testing.T) {
	st := fixture.NewTestStore()
	now := time.Now()
	st.Notes = []store.NoteRow{
		{ID: "n1", SessionID: "sess-1", Content: "a", Category: "alpha", CreatedAt: now.Add(-2 * time.Hour)},
		{ID: "n2", SessionID: "sess-1", Content: "b", Category: "beta", CreatedAt: now.Add(-1 * time.Hour)},
	}
	np := newNotepad(st)
	notes, err := np.Read(context.Background(), ReadInput{Category: "alpha"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(notes) != 1 || notes[0].Text != "a" {
		t.Errorf("category filter failed: %+v", notes)
	}
}

func TestRead_WindowCutoff(t *testing.T) {
	st := fixture.NewTestStore()
	now := time.Now()
	// One note inside the 48h window, one outside.
	st.Notes = []store.NoteRow{
		{ID: "fresh", SessionID: "sess-1", Content: "fresh", CreatedAt: now.Add(-1 * time.Hour)},
		{ID: "stale", SessionID: "sess-1", Content: "stale", CreatedAt: now.Add(-72 * time.Hour)},
	}
	np := newNotepad(st)
	notes, err := np.Read(context.Background(), ReadInput{})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(notes) != 1 || notes[0].Text != "fresh" {
		t.Errorf("window cutoff failed; got %+v", notes)
	}
}

func TestSearch_FallsBackOnErrNoEmbedder(t *testing.T) {
	st := fixture.NewTestStore() // TestStore.SearchNotes returns ErrNoEmbedder
	now := time.Now()
	st.Notes = []store.NoteRow{
		{ID: "n1", SessionID: "sess-1", Content: "alpha", CreatedAt: now.Add(-1 * time.Hour)},
	}
	np := newNotepad(st)
	notes, err := np.Search(context.Background(), SearchInput{Query: "anything"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(notes) != 1 || notes[0].Text != "alpha" {
		t.Errorf("fallback recency listing failed: %+v", notes)
	}
}

func TestSearch_EmptyQuery(t *testing.T) {
	np := newNotepad(fixture.NewTestStore())
	if _, err := np.Search(context.Background(), SearchInput{Query: ""}); err == nil {
		t.Fatal("expected error on empty query")
	}
}

func TestShow_RendersBuckets(t *testing.T) {
	st := fixture.NewTestStore()
	now := time.Now()
	st.Notes = []store.NoteRow{
		{ID: "n1", SessionID: "sess-1", Content: "schema A", Category: "schema-finding", AuthorRole: "worker", CreatedAt: now.Add(-2 * time.Hour)},
		{ID: "n2", SessionID: "sess-1", Content: "pref X", Category: "user-preference", AuthorRole: "root", CreatedAt: now.Add(-1 * time.Hour)},
	}
	np := newNotepad(st)
	out, err := np.Show(context.Background(), ShowInput{})
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if !strings.Contains(out, "## user-preference (1)") {
		t.Errorf("missing user-preference bucket header: %s", out)
	}
	if !strings.Contains(out, "## schema-finding (1)") {
		t.Errorf("missing schema-finding bucket header: %s", out)
	}
	// user-preference is newer, so its group should come first.
	if strings.Index(out, "user-preference") > strings.Index(out, "schema-finding") {
		t.Errorf("expected newer category group first; got: %s", out)
	}
}

func TestShow_EmptyMessage(t *testing.T) {
	np := newNotepad(fixture.NewTestStore())
	out, err := np.Show(context.Background(), ShowInput{})
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if !strings.Contains(out, "empty") {
		t.Errorf("expected empty-state message, got %q", out)
	}
}

func TestNew_DefaultsWindow(t *testing.T) {
	np := New(fixture.NewTestStore(), "a1", "s1", "", "", 0)
	if np.Window() != DefaultWindow {
		t.Errorf("Window = %v, want %v", np.Window(), DefaultWindow)
	}
	if np.RootID() != "s1" {
		t.Errorf("RootID = %q, want s1", np.RootID())
	}
	if np.Role() != skill.TierRoot {
		t.Errorf("Role = %q, want %q", np.Role(), skill.TierRoot)
	}
}
