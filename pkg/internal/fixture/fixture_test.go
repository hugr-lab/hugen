package fixture

import (
	"context"
	"testing"

	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

// TestRuntimeStore_ListEvents_MinSeq exercises the phase-2 cursor
// semantics on the consumer-side facade. It uses the in-memory
// fakeStore (which mirrors RuntimeStoreLocal's MinSeq behaviour) so
// the test runs without DuckDB or a Hugr endpoint.
func TestRuntimeStore_ListEvents_MinSeq(t *testing.T) {
	ctx := context.Background()
	s := NewTestStore()
	const sid = "s1"
	if err := s.OpenSession(ctx, store.SessionRow{ID: sid, AgentID: "a1", Status: store.StatusActive}); err != nil {
		t.Fatalf("open: %v", err)
	}
	const total = 10
	for i := 0; i < total; i++ {
		if err := s.AppendEvent(ctx, store.EventRow{
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
		out, err := s.ListEvents(ctx, sid, store.ListEventsOpts{})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(out) != total {
			t.Fatalf("len=%d, want %d", len(out), total)
		}
	})

	t.Run("min_seq_strict_inequality", func(t *testing.T) {
		// MinSeq=5 must skip seqs 1..5 and return seqs 6..10.
		out, err := s.ListEvents(ctx, sid, store.ListEventsOpts{MinSeq: 5})
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
		out, err := s.ListEvents(ctx, sid, store.ListEventsOpts{MinSeq: 999})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(out) != 0 {
			t.Fatalf("len=%d, want 0", len(out))
		}
	})

	t.Run("limit_caps_count", func(t *testing.T) {
		out, err := s.ListEvents(ctx, sid, store.ListEventsOpts{Limit: 3})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(out) != 3 {
			t.Fatalf("len=%d, want 3", len(out))
		}
	})

	t.Run("min_seq_and_limit_compose", func(t *testing.T) {
		// MinSeq=5, Limit=2 → seqs 6 and 7.
		out, err := s.ListEvents(ctx, sid, store.ListEventsOpts{MinSeq: 5, Limit: 2})
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
	s := NewTestStore()
	mustOpen := func(id, parent string) {
		t.Helper()
		row := store.SessionRow{ID: id, AgentID: "a1", Status: store.StatusActive, ParentSessionID: parent}
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
