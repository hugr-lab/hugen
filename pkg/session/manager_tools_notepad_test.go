package session

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestCallNotepadAppend_Happy(t *testing.T) {
	store := newFakeStore()
	mgr := newTestManager(t, store)
	defer mgr.ShutdownAll(context.Background())

	parent := us1OpenParent(t, mgr)

	args, _ := json.Marshal(notepadAppendInput{Text: "remember this"})
	out, err := callNotepadAppend(us1WithSession(parent), mgr, args)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["id"] == "" {
		t.Fatalf("empty note id, out=%s", out)
	}

	notes, err := parent.Notepad().List(context.Background(), 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(notes) != 1 || notes[0].Text != "remember this" {
		t.Errorf("notes = %v, want one entry with our text", notes)
	}
}

func TestCallNotepadAppend_BadRequest(t *testing.T) {
	store := newFakeStore()
	mgr := newTestManager(t, store)
	defer mgr.ShutdownAll(context.Background())
	parent := us1OpenParent(t, mgr)

	out, err := callNotepadAppend(us1WithSession(parent), mgr, json.RawMessage(`{not-json`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !strings.Contains(string(out), `"code":"bad_request"`) {
		t.Errorf("expected bad_request error, got %s", out)
	}
}

func TestCallNotepadAppend_EmptyText(t *testing.T) {
	store := newFakeStore()
	mgr := newTestManager(t, store)
	defer mgr.ShutdownAll(context.Background())
	parent := us1OpenParent(t, mgr)

	args, _ := json.Marshal(notepadAppendInput{Text: ""})
	out, err := callNotepadAppend(us1WithSession(parent), mgr, args)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !strings.Contains(string(out), `"code":"io"`) {
		t.Errorf("expected io error from empty text, got %s", out)
	}
}

func TestNotepadAppend_RegisteredOnManager(t *testing.T) {
	store := newFakeStore()
	mgr := newTestManager(t, store)
	defer mgr.ShutdownAll(context.Background())

	tools, err := mgr.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, tt := range tools {
		if tt.Name == "session:notepad_append" {
			return
		}
	}
	names := make([]string, 0, len(tools))
	for _, tt := range tools {
		names = append(names, tt.Name)
	}
	t.Errorf("session:notepad_append not registered on Manager (have: %v)", names)
}
