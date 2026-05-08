package store

import (
	"testing"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// TestFrameToEventRow_RoundTrip_Phase4 asserts every phase-4 Frame
// kind survives FrameToEventRow → EventRowToFrame, including
// envelope-field round-trip (FromSession, RequestID).
func TestFrameToEventRow_RoundTrip_Phase4(t *testing.T) {
	author := protocol.ParticipantInfo{ID: "agent", Kind: protocol.ParticipantAgent}
	cases := []struct {
		name string
		make func() protocol.Frame
	}{
		{"subagent_started", func() protocol.Frame {
			return protocol.NewSubagentStarted("p", author, protocol.SubagentStartedPayload{
				ChildSessionID: "c", Skill: "hugr-data", Role: "explorer",
				Task: "list sources", Depth: 1,
			})
		}},
		{"subagent_result", func() protocol.Frame {
			return protocol.NewSubagentResult("p", "c", author, protocol.SubagentResultPayload{
				SessionID: "c", Result: "ok", Reason: protocol.TerminationCompleted, TurnsUsed: 4,
			})
		}},
		{"extension_frame_plan_set", func() protocol.Frame {
			return protocol.NewExtensionFrame("s", author, "plan",
				protocol.CategoryOp, "set",
				[]byte(`{"text":"## plan","current_step":"step 1"}`))
		}},
		{"extension_frame_plan_clear", func() protocol.Frame {
			return protocol.NewExtensionFrame("s", author, "plan",
				protocol.CategoryOp, "clear", nil)
		}},
		{"extension_frame_whiteboard_init", func() protocol.Frame {
			return protocol.NewExtensionFrame("h", author, "whiteboard",
				protocol.CategoryOp, "init", nil)
		}},
		{"extension_frame_whiteboard_write", func() protocol.Frame {
			f := protocol.NewExtensionFrame("h", author, "whiteboard",
				protocol.CategoryOp, "write",
				[]byte(`{"seq":7,"from_session_id":"c","from_role":"explorer","text":"found x"}`))
			f.BaseFrame.FromSession = "c"
			return f
		}},
		{"extension_frame_whiteboard_message", func() protocol.Frame {
			f := protocol.NewExtensionFrame("r", author, "whiteboard",
				protocol.CategoryMessage, "message",
				[]byte(`{"seq":7,"from_session_id":"c","from_role":"explorer","text":"found x"}`))
			f.BaseFrame.FromSession = "h"
			return f
		}},
		{"session_terminated", func() protocol.Frame {
			return protocol.NewSessionTerminated("s", author, protocol.SessionTerminatedPayload{
				Reason: protocol.TerminationHardCeiling, TurnsUsed: 30,
			})
		}},
		{"system_message", func() protocol.Frame {
			return protocol.NewSystemMessage("s", author, protocol.SystemMessageWhiteboard,
				"[whiteboard] explorer (c): found x")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := c.make()
			row, _, err := FrameToEventRow(in, "a1")
			if err != nil {
				t.Fatalf("to row: %v", err)
			}
			if row.EventType != string(in.Kind()) {
				t.Errorf("event_type=%q, want %q", row.EventType, in.Kind())
			}
			out, err := EventRowToFrame(row)
			if err != nil {
				t.Fatalf("from row: %v", err)
			}
			if out.Kind() != in.Kind() {
				t.Errorf("kind drift: %q != %q", out.Kind(), in.Kind())
			}
			if out.FromSessionID() != in.FromSessionID() {
				t.Errorf("FromSession drift: %q != %q", out.FromSessionID(), in.FromSessionID())
			}
		})
	}
}

// TestFrameToEventRow_EnvelopeOverlay_RoundTrip asserts the
// FromParticipant + RequestID reservations also round-trip cleanly
// even though phase-4 producers don't fill them.
func TestFrameToEventRow_EnvelopeOverlay_RoundTrip(t *testing.T) {
	author := protocol.ParticipantInfo{ID: "agent", Kind: protocol.ParticipantAgent}
	in := protocol.NewSubagentResult("p", "c", author, protocol.SubagentResultPayload{
		SessionID: "c", Result: "ok", Reason: protocol.TerminationCompleted, TurnsUsed: 1,
	})
	in.RequestID = "req-7"
	in.FromParticipant = "alice"
	row, _, err := FrameToEventRow(in, "a1")
	if err != nil {
		t.Fatalf("to row: %v", err)
	}
	out, err := EventRowToFrame(row)
	if err != nil {
		t.Fatalf("from row: %v", err)
	}
	got, ok := out.(*protocol.SubagentResult)
	if !ok {
		t.Fatalf("type %T, want *protocol.SubagentResult", out)
	}
	if got.RequestID != "req-7" || got.FromParticipant != "alice" || got.FromSession != "c" {
		t.Errorf("envelope drift: %+v", got.BaseFrame)
	}
	if got.Payload.Reason != protocol.TerminationCompleted {
		t.Errorf("payload drift: %+v", got.Payload)
	}
}
