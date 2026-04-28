package protocol

import (
	"errors"
	"testing"
)

var testAuthor = ParticipantInfo{ID: "u1", Kind: ParticipantUser, Name: "alice"}
var testAgent = ParticipantInfo{ID: "a1", Kind: ParticipantAgent, Name: "hugen"}

func TestCodec_RoundTrip(t *testing.T) {
	codec := NewCodec()
	cases := []struct {
		name string
		in   Frame
	}{
		{"user_message", NewUserMessage("s1", testAuthor, "hello")},
		{"agent_message", NewAgentMessage("s1", testAgent, "hi", 0, true)},
		{"reasoning", NewReasoning("s1", testAgent, "thinking", 0, false)},
		{"tool_call", NewToolCall("s1", testAgent, "t1", "memory_note", map[string]any{"text": "x"})},
		{"tool_result", NewToolResult("s1", testAgent, "t1", "ok", false)},
		{"slash_command", NewSlashCommand("s1", testAuthor, "note", []string{"hi"}, "/note hi")},
		{"cancel", NewCancel("s1", testAuthor, "user_cancelled")},
		{"session_opened", NewSessionOpened("s1", testAgent, []ParticipantInfo{testAuthor, testAgent})},
		{"session_closed", NewSessionClosed("s1", testAgent, "user_end")},
		{"session_suspended", NewSessionSuspended("s1", testAgent)},
		{"heartbeat", NewHeartbeat("s1", testAgent)},
		{"error", NewError("s1", testAgent, "model_unavailable", "no model", true)},
		{"system_marker", NewSystemMarker("s1", testAgent, "note_added", map[string]any{"id": "n1"})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data, err := codec.EncodeFrame(c.in)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			out, err := codec.DecodeFrame(data)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if out.FrameID() != c.in.FrameID() {
				t.Errorf("frame_id mismatch: %s != %s", out.FrameID(), c.in.FrameID())
			}
			if out.Kind() != c.in.Kind() {
				t.Errorf("kind mismatch: %s != %s", out.Kind(), c.in.Kind())
			}
			if out.SessionID() != c.in.SessionID() {
				t.Errorf("session mismatch")
			}
			if out.Author().ID != c.in.Author().ID {
				t.Errorf("author mismatch")
			}
		})
	}
}

func TestCodec_DecodeUnknownKind(t *testing.T) {
	codec := NewCodec()
	data := []byte(`{"frame_id":"x","session_id":"s","kind":"bogus","author":{"id":"u","kind":"user"},"occurred_at":"2026-01-01T00:00:00Z","payload":{}}`)
	if _, err := codec.DecodeFrame(data); !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("expected ErrUnknownKind, got %v", err)
	}
}

func TestCodec_DecodeMalformedJSON(t *testing.T) {
	codec := NewCodec()
	if _, err := codec.DecodeFrame([]byte(`{not json`)); !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("expected ErrInvalidPayload, got %v", err)
	}
}

func TestValidate_RejectsEmptySessionID(t *testing.T) {
	f := NewUserMessage("", testAuthor, "x")
	if err := Validate(f); err == nil {
		t.Fatal("expected error for empty session_id")
	}
}

func TestValidate_AllowsEmptyAgentMessage(t *testing.T) {
	// Empty text is valid on both final and non-final agent_messages.
	// Final-empty is the end-of-turn marker after streamed deltas;
	// non-final empty can occur as a streaming heartbeat.
	for _, final := range []bool{true, false} {
		f := NewAgentMessage("s1", testAgent, "", 0, final)
		if err := Validate(f); err != nil {
			t.Errorf("final=%v: empty agent_message must be valid: %v", final, err)
		}
	}
}

func TestValidate_RejectsEmptySlashCommandName(t *testing.T) {
	f := NewSlashCommand("s1", testAuthor, "", nil, "/")
	if err := Validate(f); err == nil {
		t.Fatal("expected error for empty slash command name")
	}
}

func TestEncodePayload_OmitsEnvelope(t *testing.T) {
	codec := NewCodec()
	f := NewUserMessage("s1", testAuthor, "hello")
	data, err := codec.EncodePayload(f)
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	if string(data) == "" {
		t.Fatal("empty payload bytes")
	}
	if string(data) != `{"text":"hello"}` {
		t.Fatalf("unexpected payload bytes: %s", data)
	}
}
