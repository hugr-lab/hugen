package notepad

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	notepadpkg "github.com/hugr-lab/hugen/pkg/session/tools/notepad"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// fakeStore is the minimal notepad.Store the handler needs —
// records appended rows in memory + serves them back via List.
type fakeStore struct {
	mu   sync.Mutex
	rows []notepadpkg.NoteRow
}

func (f *fakeStore) AppendNote(_ context.Context, row notepadpkg.NoteRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = append(f.rows, row)
	return nil
}

func (f *fakeStore) ListNotes(_ context.Context, sessionID string, _ int) ([]notepadpkg.NoteRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]notepadpkg.NoteRow, 0, len(f.rows))
	for _, r := range f.rows {
		if r.SessionID == sessionID {
			out = append(out, r)
		}
	}
	return out, nil
}

// fakeState implements tool.SessionState — sync.Map-backed Value/
// SetValue plus stable SessionID/ParentID strings. Used by the
// handler tests so they don't need a full *session.Session.
type fakeState struct {
	id, parent string
	state      sync.Map
}

func (f *fakeState) SessionID() string { return f.id }
func (f *fakeState) ParentID() string  { return f.parent }
func (f *fakeState) SetValue(name string, value any) {
	f.state.Store(name, value)
}
func (f *fakeState) Value(name string) (any, bool) {
	return f.state.Load(name)
}
func (f *fakeState) ParentValue(_ string) (any, bool) { return nil, false }

// newFixture builds an Extension + a state seeded by InitState so
// every test starts from "fresh, ready-to-call" — same shape the
// runtime gives a brand-new session.
func newFixture(t *testing.T) (*Extension, *fakeState, *fakeStore) {
	t.Helper()
	store := &fakeStore{}
	ext := New(store, "agent-test")
	state := &fakeState{id: "ses-test"}
	if err := ext.InitState(context.Background(), state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	return ext, state, store
}

func TestExtension_Name(t *testing.T) {
	ext := New(&fakeStore{}, "a1")
	if got := ext.Name(); got != "notepad" {
		t.Errorf("Name = %q, want notepad", got)
	}
}

func TestExtension_List(t *testing.T) {
	ext := New(&fakeStore{}, "a1")
	tools, err := ext.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("len(tools) = %d, want 1", len(tools))
	}
	if tools[0].Name != "notepad:append" {
		t.Errorf("tool name = %q, want notepad:append", tools[0].Name)
	}
	if tools[0].PermissionObject != PermObject {
		t.Errorf("perm object = %q, want %q", tools[0].PermissionObject, PermObject)
	}
}

func TestExtension_InitState_StashesNotepad(t *testing.T) {
	_, state, _ := newFixture(t)
	np := FromState(state)
	if np == nil {
		t.Fatal("FromState returned nil after InitState")
	}
}

// TestCallAppend_Happy drives the tool through Call (the
// dispatcher path) and verifies the row landed in the store.
func TestCallAppend_Happy(t *testing.T) {
	ext, state, store := newFixture(t)
	ctx := tool.WithSessionState(context.Background(), state)

	args, _ := json.Marshal(appendInput{Text: "remember this"})
	out, err := ext.Call(ctx, "notepad:append", args)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["id"] == "" {
		t.Fatalf("empty id; out=%s", out)
	}

	if len(store.rows) != 1 || store.rows[0].Content != "remember this" {
		t.Errorf("store rows = %+v, want one with our text", store.rows)
	}
}

func TestCallAppend_BadRequest(t *testing.T) {
	ext, state, _ := newFixture(t)
	ctx := tool.WithSessionState(context.Background(), state)

	out, err := ext.Call(ctx, "notepad:append", json.RawMessage(`{not-json`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(string(out), `"code":"bad_request"`) {
		t.Errorf("expected bad_request, got %s", out)
	}
}

func TestCallAppend_EmptyText(t *testing.T) {
	ext, state, _ := newFixture(t)
	ctx := tool.WithSessionState(context.Background(), state)

	args, _ := json.Marshal(appendInput{Text: ""})
	out, err := ext.Call(ctx, "notepad:append", args)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(string(out), `"code":"io"`) {
		t.Errorf("expected io error from empty text, got %s", out)
	}
}

func TestCallAppend_NoSessionInContext(t *testing.T) {
	ext := New(&fakeStore{}, "a1")
	args, _ := json.Marshal(appendInput{Text: "x"})
	out, err := ext.Call(context.Background(), "notepad:append", args)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(string(out), `"code":"session_gone"`) {
		t.Errorf("expected session_gone, got %s", out)
	}
}

func TestCallAppend_UnknownOp(t *testing.T) {
	ext, state, _ := newFixture(t)
	ctx := tool.WithSessionState(context.Background(), state)

	if _, err := ext.Call(ctx, "notepad:nope", json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected error for unknown op")
	}
}
