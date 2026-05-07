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
		{"plan_op_set", func() protocol.Frame {
			return protocol.NewPlanOp("s", author, protocol.PlanOpPayload{
				Op: "set", Text: "## plan", CurrentStep: "step 1",
			})
		}},
		{"plan_op_clear", func() protocol.Frame {
			return protocol.NewPlanOp("s", author, protocol.PlanOpPayload{Op: "clear"})
		}},
		{"whiteboard_op_init", func() protocol.Frame {
			return protocol.NewWhiteboardOp("h", "", author, protocol.WhiteboardOpPayload{Op: "init"})
		}},
		{"whiteboard_op_write", func() protocol.Frame {
			return protocol.NewWhiteboardOp("h", "c", author, protocol.WhiteboardOpPayload{
				Op: "write", Seq: 7, FromSessionID: "c", FromRole: "explorer", Text: "found x",
			})
		}},
		{"whiteboard_message", func() protocol.Frame {
			return protocol.NewWhiteboardMessage("h", "r", author, protocol.WhiteboardMessagePayload{
				FromSessionID: "c", FromRole: "explorer", Seq: 7, Text: "found x",
			})
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
