package session

import (
	"context"
	"testing"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// TestRuntimeStore_ListEvents_MinSeq exercises the phase-2 cursor
// semantics on the consumer-side facade. It uses the in-memory
// fakeStore (which mirrors RuntimeStoreLocal's MinSeq behaviour) so
// the test runs without DuckDB or a Hugr endpoint.
func TestRuntimeStore_ListEvents_MinSeq(t *testing.T) {
	ctx := context.Background()
	s := newFakeStore()
	const sid = "s1"
	if err := s.OpenSession(ctx, SessionRow{ID: sid, AgentID: "a1", Status: StatusActive}); err != nil {
		t.Fatalf("open: %v", err)
	}
	const total = 10
	for i := 0; i < total; i++ {
		if err := s.AppendEvent(ctx, EventRow{
			ID:        "ev-" + string(rune('a'+i)),
			SessionID: sid,
			AgentID:   "a1",
			EventType: string(protocol.KindUserMessage),
			Author:    "u1",
			Content:   "msg",
		}, ""); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	t.Run("min_seq_zero_returns_all", func(t *testing.T) {
		out, err := s.ListEvents(ctx, sid, ListEventsOpts{})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(out) != total {
			t.Fatalf("len=%d, want %d", len(out), total)
		}
	})

	t.Run("min_seq_strict_inequality", func(t *testing.T) {
		// MinSeq=5 must skip seqs 1..5 and return seqs 6..10.
		out, err := s.ListEvents(ctx, sid, ListEventsOpts{MinSeq: 5})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(out) != total-5 {
			t.Fatalf("len=%d, want %d", len(out), total-5)
		}
		for i, ev := range out {
			wantSeq := 5 + i + 1
			if ev.Seq != wantSeq {
				t.Errorf("out[%d].Seq=%d, want %d", i, ev.Seq, wantSeq)
			}
		}
	})

	t.Run("min_seq_beyond_max_returns_empty", func(t *testing.T) {
		out, err := s.ListEvents(ctx, sid, ListEventsOpts{MinSeq: 999})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(out) != 0 {
			t.Fatalf("len=%d, want 0", len(out))
		}
	})

	t.Run("limit_caps_count", func(t *testing.T) {
		out, err := s.ListEvents(ctx, sid, ListEventsOpts{Limit: 3})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(out) != 3 {
			t.Fatalf("len=%d, want 3", len(out))
		}
	})

	t.Run("min_seq_and_limit_compose", func(t *testing.T) {
		// MinSeq=5, Limit=2 → seqs 6 and 7.
		out, err := s.ListEvents(ctx, sid, ListEventsOpts{MinSeq: 5, Limit: 2})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(out) != 2 {
			t.Fatalf("len=%d, want 2", len(out))
		}
		if out[0].Seq != 6 || out[1].Seq != 7 {
			t.Errorf("seqs=%d,%d, want 6,7", out[0].Seq, out[1].Seq)
		}
	})
}

// TestRuntimeStore_ListChildren covers the phase-4 BFS-walker query.
func TestRuntimeStore_ListChildren(t *testing.T) {
	ctx := context.Background()
	s := newFakeStore()
	mustOpen := func(id, parent string) {
		t.Helper()
		row := SessionRow{ID: id, AgentID: "a1", Status: StatusActive, ParentSessionID: parent}
		if err := s.OpenSession(ctx, row); err != nil {
			t.Fatalf("open %s: %v", id, err)
		}
	}
	mustOpen("root", "")
	mustOpen("c1", "root")
	mustOpen("c2", "root")
	mustOpen("g1", "c1")

	t.Run("two_direct_children", func(t *testing.T) {
		out, err := s.ListChildren(ctx, "root")
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(out) != 2 {
			t.Fatalf("len=%d, want 2", len(out))
		}
		ids := map[string]bool{out[0].ID: true, out[1].ID: true}
		if !ids["c1"] || !ids["c2"] {
			t.Errorf("missing child: %v", ids)
		}
	})

	t.Run("nested_grandchild_excluded", func(t *testing.T) {
		out, err := s.ListChildren(ctx, "c1")
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(out) != 1 || out[0].ID != "g1" {
			t.Errorf("got %v, want [g1]", out)
		}
	})

	t.Run("leaf_returns_empty", func(t *testing.T) {
		out, err := s.ListChildren(ctx, "g1")
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(out) != 0 {
			t.Errorf("got %d, want 0", len(out))
		}
	})
}

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
