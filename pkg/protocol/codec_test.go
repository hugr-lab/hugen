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
		{"heartbeat", NewHeartbeat("s1", testAgent)},
		{"error", NewError("s1", testAgent, "model_unavailable", "no model", true)},
		{"system_marker", NewSystemMarker("s1", testAgent, "note_added", map[string]any{"id": "n1"})},
		{"subagent_started", NewSubagentStarted("s1", testAgent, SubagentStartedPayload{
			ChildSessionID: "s2", Skill: "hugr-data", Role: "explorer",
			Task: "list sources", Depth: 1,
			Inputs: map[string]any{"hint": "x"},
		})},
		{"subagent_result", NewSubagentResult("s1", "s2", testAgent, SubagentResultPayload{
			SessionID: "s2", Result: "found 3 tables",
			Reason: TerminationCompleted, TurnsUsed: 4,
		})},
		{"plan_op_set", NewPlanOp("s1", testAgent, PlanOpPayload{
			Op: "set", Text: "# Plan\n1. discover\n2. aggregate", CurrentStep: "step 1",
		})},
		{"plan_op_comment", NewPlanOp("s1", testAgent, PlanOpPayload{
			Op: "comment", Text: "found A,B,C", CurrentStep: "step 2",
		})},
		{"plan_op_clear", NewPlanOp("s1", testAgent, PlanOpPayload{Op: "clear"})},
		{"whiteboard_op_init", NewWhiteboardOp("s1", "", testAgent, WhiteboardOpPayload{Op: "init"})},
		{"whiteboard_op_write", NewWhiteboardOp("s1", "s2", testAgent, WhiteboardOpPayload{
			Op: "write", Seq: 7, FromSessionID: "s2", FromRole: "explorer", Text: "found auth_logs",
		})},
		{"whiteboard_op_stop", NewWhiteboardOp("s1", "", testAgent, WhiteboardOpPayload{Op: "stop"})},
		{"whiteboard_message", NewWhiteboardMessage("s1", "s3", testAgent, WhiteboardMessagePayload{
			FromSessionID: "s2", FromRole: "explorer", Seq: 7, Text: "found auth_logs",
		})},
		{"session_terminated", NewSessionTerminated("s1", testAgent, SessionTerminatedPayload{
			Reason: TerminationHardCeiling, TurnsUsed: 30,
		})},
		{"session_close", NewSessionClose("s1", testAgent, TerminationHardCeiling)},
		{"system_message_soft_warning", NewSystemMessage("s1", testAgent,
			SystemMessageSoftWarning, "you have used N turns")},
		{"system_message_whiteboard", NewSystemMessage("s1", testAgent,
			SystemMessageWhiteboard, "[whiteboard] explorer (s2): found auth_logs")},
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

// TestCodec_RoundTrip_EnvelopeAdditions asserts the phase-4 BaseFrame
// fields (FromSession, FromParticipant, RequestID) round-trip cleanly
// when set and remain absent (omitempty) when empty.
func TestCodec_RoundTrip_EnvelopeAdditions(t *testing.T) {
	codec := NewCodec()
	t.Run("set", func(t *testing.T) {
		f := NewSubagentResult("parent", "child", testAgent, SubagentResultPayload{
			SessionID: "child", Result: "ok", Reason: TerminationCompleted, TurnsUsed: 2,
		})
		f.RequestID = "req-7"
		f.FromParticipant = "human-alice"
		data, err := codec.EncodeFrame(f)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		out, err := codec.DecodeFrame(data)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		got, ok := out.(*SubagentResult)
		if !ok {
			t.Fatalf("decoded type %T, want *SubagentResult", out)
		}
		if got.FromSession != "child" {
			t.Errorf("FromSession = %q, want child", got.FromSession)
		}
		if got.RequestID != "req-7" {
			t.Errorf("RequestID = %q, want req-7", got.RequestID)
		}
		if got.FromParticipant != "human-alice" {
			t.Errorf("FromParticipant = %q, want human-alice", got.FromParticipant)
		}
		if got.Payload.SessionID != "child" || got.Payload.Reason != TerminationCompleted ||
			got.Payload.TurnsUsed != 2 || got.Payload.Result != "ok" {
			t.Errorf("payload drift: %+v", got.Payload)
		}
	})
	t.Run("omitted_when_empty", func(t *testing.T) {
		f := NewUserMessage("s1", testAuthor, "hi")
		data, err := codec.EncodeFrame(f)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		// omitempty: the JSON body must not contain the optional fields.
		s := string(data)
		for _, key := range []string{"from_session", "from_participant", "request_id"} {
			if contains(s, key) {
				t.Errorf("envelope unexpectedly contains %q for empty value: %s", key, s)
			}
		}
	})
}

// TestCodec_RoundTrip_CancelCascade asserts the phase-4 Cancel.Cascade
// flag round-trips and is absent when false.
func TestCodec_RoundTrip_CancelCascade(t *testing.T) {
	codec := NewCodec()
	t.Run("cascade_true", func(t *testing.T) {
		f := NewCancel("s1", testAuthor, "user_cancelled")
		f.Payload.Cascade = true
		data, err := codec.EncodeFrame(f)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		out, err := codec.DecodeFrame(data)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		got, ok := out.(*Cancel)
		if !ok {
			t.Fatalf("decoded %T, want *Cancel", out)
		}
		if !got.Payload.Cascade {
			t.Errorf("Cascade lost in round-trip")
		}
	})
	t.Run("cascade_false_omitted", func(t *testing.T) {
		f := NewCancel("s1", testAuthor, "user_cancelled")
		data, err := codec.EncodeFrame(f)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if contains(string(data), `"cascade"`) {
			t.Errorf("cascade=false should be omitted from JSON: %s", data)
		}
	})
}

// TestCodec_PayloadIntegrity_Phase4 asserts every phase-4 payload's
// fields survive a full encode→decode cycle.
func TestCodec_PayloadIntegrity_Phase4(t *testing.T) {
	codec := NewCodec()
	t.Run("subagent_started", func(t *testing.T) {
		in := NewSubagentStarted("p", testAgent, SubagentStartedPayload{
			ChildSessionID: "c", Skill: "hugr-data", Role: "explorer",
			Task: "do thing", Depth: 2, Inputs: map[string]any{"k": float64(1)},
			ParentWhiteboardActive: true,
		})
		data, _ := codec.EncodeFrame(in)
		out, err := codec.DecodeFrame(data)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		got := out.(*SubagentStarted).Payload
		if got.ChildSessionID != "c" || got.Skill != "hugr-data" || got.Role != "explorer" ||
			got.Task != "do thing" || got.Depth != 2 || !got.ParentWhiteboardActive {
			t.Errorf("payload drift: %+v", got)
		}
	})
	t.Run("plan_op_clear_no_text", func(t *testing.T) {
		in := NewPlanOp("s", testAgent, PlanOpPayload{Op: "clear"})
		data, _ := codec.EncodeFrame(in)
		out, _ := codec.DecodeFrame(data)
		got := out.(*PlanOp).Payload
		if got.Op != "clear" || got.Text != "" || got.CurrentStep != "" {
			t.Errorf("clear payload drift: %+v", got)
		}
	})
	t.Run("whiteboard_op_truncated_flag", func(t *testing.T) {
		in := NewWhiteboardOp("h", "c", testAgent, WhiteboardOpPayload{
			Op: "write", Seq: 99, FromSessionID: "c", FromRole: "x", Text: "y", Truncated: true,
		})
		data, _ := codec.EncodeFrame(in)
		out, _ := codec.DecodeFrame(data)
		got := out.(*WhiteboardOp).Payload
		if !got.Truncated || got.Seq != 99 {
			t.Errorf("write payload drift: %+v", got)
		}
	})
	t.Run("session_terminated_with_result", func(t *testing.T) {
		in := NewSessionTerminated("s", testAgent, SessionTerminatedPayload{
			Reason: TerminationCompleted, Result: "final answer", TurnsUsed: 3,
		})
		data, _ := codec.EncodeFrame(in)
		out, _ := codec.DecodeFrame(data)
		got := out.(*SessionTerminated).Payload
		if got.Reason != TerminationCompleted || got.Result != "final answer" || got.TurnsUsed != 3 {
			t.Errorf("payload drift: %+v", got)
		}
	})
}

// TestValidate_Phase4 covers per-variant validation.
func TestValidate_Phase4(t *testing.T) {
	cases := []struct {
		name    string
		f       Frame
		wantErr bool
	}{
		{"subagent_started_ok", NewSubagentStarted("p", testAgent, SubagentStartedPayload{
			ChildSessionID: "c", Task: "t", Depth: 1,
		}), false},
		{"subagent_started_missing_task", NewSubagentStarted("p", testAgent, SubagentStartedPayload{
			ChildSessionID: "c", Depth: 1,
		}), true},
		{"subagent_result_missing_reason", NewSubagentResult("p", "c", testAgent, SubagentResultPayload{
			SessionID: "c",
		}), true},
		{"plan_op_invalid_op", NewPlanOp("s", testAgent, PlanOpPayload{Op: "rename"}), true},
		{"whiteboard_op_invalid_op", NewWhiteboardOp("s", "", testAgent, WhiteboardOpPayload{Op: "spin"}), true},
		{"whiteboard_message_missing_from", NewWhiteboardMessage("h", "r", testAgent, WhiteboardMessagePayload{}), true},
		{"session_terminated_missing_reason", NewSessionTerminated("s", testAgent, SessionTerminatedPayload{}), true},
		{"system_message_missing_kind", NewSystemMessage("s", testAgent, "", "x"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Validate(c.f)
			if c.wantErr && err == nil {
				t.Errorf("expected error, got nil")
			}
			if !c.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

// contains is a small helper to avoid importing strings into a test
// already light on imports.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestCodec_DecodeUnknownKind_AsOpaque(t *testing.T) {
	// Phase 2 (R-Plan-26 / FR-024): unknown but non-empty kinds
	// round-trip as *OpaqueFrame instead of failing.
	codec := NewCodec()
	data := []byte(`{"frame_id":"x","session_id":"s","kind":"sub_agent_spawn","author":{"id":"u","kind":"user"},"occurred_at":"2026-01-01T00:00:00Z","payload":{"foo":"bar"}}`)
	out, err := codec.DecodeFrame(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	op, ok := out.(*OpaqueFrame)
	if !ok {
		t.Fatalf("expected *OpaqueFrame, got %T", out)
	}
	if op.KindRaw != "sub_agent_spawn" {
		t.Errorf("KindRaw = %q, want sub_agent_spawn", op.KindRaw)
	}
	if op.Kind() != Kind("sub_agent_spawn") {
		t.Errorf("Kind() = %q, want sub_agent_spawn", op.Kind())
	}
	if string(op.RawPayload) != `{"foo":"bar"}` {
		t.Errorf("RawPayload = %s, want {\"foo\":\"bar\"}", op.RawPayload)
	}
}

func TestCodec_DecodeEmptyKind_StillError(t *testing.T) {
	codec := NewCodec()
	data := []byte(`{"frame_id":"x","session_id":"s","kind":"","author":{"id":"u","kind":"user"},"occurred_at":"2026-01-01T00:00:00Z","payload":{}}`)
	if _, err := codec.DecodeFrame(data); !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("expected ErrUnknownKind for empty kind, got %v", err)
	}
}

// TestCodec_RoundTrip_OpaqueFrame asserts every phase-deferred kind
// listed in contracts/sse-wire-format.md §"Variants on the wire"
// survives encode→decode→encode byte-for-byte. (FR-024 / SC-016)
func TestCodec_RoundTrip_OpaqueFrame(t *testing.T) {
	codec := NewCodec()
	deferredKinds := []string{
		"sub_agent_spawn",
		"sub_agent_message",
		"sub_agent_result",
		"approval_request",
		"approval_decision",
		"clarification_request",
		"clarification_response",
		"wiki_published",
		"cron_triggered",
		"compaction_summary",
		"signal",
	}
	for _, kind := range deferredKinds {
		t.Run(kind, func(t *testing.T) {
			payload := []byte(`{"k":"` + kind + `","n":42,"nested":{"a":["x","y"]}}`)
			data := []byte(`{"frame_id":"f-` + kind + `","session_id":"s1","kind":"` + kind + `","author":{"id":"a1","kind":"agent"},"occurred_at":"2026-04-28T15:00:00Z","payload":` + string(payload) + `}`)
			out, err := codec.DecodeFrame(data)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			op, ok := out.(*OpaqueFrame)
			if !ok {
				t.Fatalf("expected *OpaqueFrame, got %T", out)
			}
			if string(op.RawPayload) != string(payload) {
				t.Errorf("RawPayload not byte-identical:\n got: %s\nwant: %s", op.RawPayload, payload)
			}
			// Round-trip through Encode and assert envelope re-emits
			// the same kind + payload.
			reencoded, err := codec.EncodeFrame(op)
			if err != nil {
				t.Fatalf("re-encode: %v", err)
			}
			out2, err := codec.DecodeFrame(reencoded)
			if err != nil {
				t.Fatalf("re-decode: %v", err)
			}
			op2, ok := out2.(*OpaqueFrame)
			if !ok {
				t.Fatalf("re-decode produced %T, want *OpaqueFrame", out2)
			}
			if op2.Kind() != Kind(kind) {
				t.Errorf("kind drift: %q", op2.Kind())
			}
			if string(op2.RawPayload) != string(payload) {
				t.Errorf("RawPayload drift after re-encode:\n got: %s\nwant: %s", op2.RawPayload, payload)
			}
		})
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
