package notepad

import (
	"context"
	"testing"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/internal/fixture"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

func testCommandContext() extension.CommandContext {
	return extension.CommandContext{
		Author:      protocol.ParticipantInfo{ID: "user-1", Kind: protocol.ParticipantUser, Name: "user"},
		AgentAuthor: protocol.ParticipantInfo{ID: "agent-1", Kind: protocol.ParticipantAgent, Name: "agent"},
	}
}

func TestExtension_Commands_Listed(t *testing.T) {
	ext := NewExtension(fixture.NewTestStore(), "a1")
	cmds := ext.Commands()
	if len(cmds) != 1 {
		t.Fatalf("len(Commands) = %d, want 1", len(cmds))
	}
	if cmds[0].Name != "note" {
		t.Errorf("cmd.Name = %q, want note", cmds[0].Name)
	}
	if cmds[0].Handler == nil {
		t.Error("cmd.Handler is nil")
	}
}

func TestCmdNote_Happy(t *testing.T) {
	ext, state, store := newFixture(t)
	frames, err := ext.cmdNote(context.Background(), state, testCommandContext(), []string{"hello", "world"})
	if err != nil {
		t.Fatalf("cmdNote: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("frames=%d, want 1", len(frames))
	}
	if frames[0].Kind() != protocol.KindSystemMarker {
		t.Fatalf("frame kind = %q, want %q", frames[0].Kind(), protocol.KindSystemMarker)
	}
	marker, ok := frames[0].(*protocol.SystemMarker)
	if !ok {
		t.Fatalf("frame type = %T, want *protocol.SystemMarker", frames[0])
	}
	if marker.Payload.Subject != "note_added" {
		t.Errorf("marker.Subject = %q, want note_added", marker.Payload.Subject)
	}
	if _, ok := marker.Payload.Details["note_id"]; !ok {
		t.Errorf("marker.Details missing note_id: %+v", marker.Payload.Details)
	}
	if len(store.Notes) != 1 || store.Notes[0].Content != "hello world" {
		t.Errorf("store.Notes = %+v, want one row with %q", store.Notes, "hello world")
	}
}

func TestCmdNote_EmptyArgs(t *testing.T) {
	ext, state, _ := newFixture(t)
	frames, err := ext.cmdNote(context.Background(), state, testCommandContext(), nil)
	if err != nil {
		t.Fatalf("cmdNote: %v", err)
	}
	if len(frames) != 1 || frames[0].Kind() != protocol.KindError {
		t.Fatalf("frames=%+v, want one error frame", frames)
	}
	errFrame, ok := frames[0].(*protocol.Error)
	if !ok {
		t.Fatalf("frame type = %T, want *protocol.Error", frames[0])
	}
	if errFrame.Payload.Code != "empty_note" {
		t.Errorf("error code = %q, want empty_note", errFrame.Payload.Code)
	}
}

func TestCmdNote_NoStateHandle(t *testing.T) {
	// Bare state without InitState — FromState returns nil.
	ext := NewExtension(fixture.NewTestStore(), "a1")
	state := fixture.NewTestSessionState("ses-bare")
	frames, err := ext.cmdNote(context.Background(), state, testCommandContext(), []string{"x"})
	if err != nil {
		t.Fatalf("cmdNote: %v", err)
	}
	if len(frames) != 1 || frames[0].Kind() != protocol.KindError {
		t.Fatalf("frames=%+v, want one error frame", frames)
	}
	errFrame, ok := frames[0].(*protocol.Error)
	if !ok {
		t.Fatalf("frame type = %T, want *protocol.Error", frames[0])
	}
	if errFrame.Payload.Code != "note_failed" {
		t.Errorf("error code = %q, want note_failed", errFrame.Payload.Code)
	}
}
