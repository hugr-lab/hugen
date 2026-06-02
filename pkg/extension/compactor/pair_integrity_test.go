package compactor

import (
	"context"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// Phase 5.2 budget-termination, Part 1 — pair-integrity tests.
//
// The model-visible history must never begin on a tool_result whose
// owning tool_call was pruned away, and a single assistant message
// bearing N parallel tool_calls (Gemini) keeps all N tool_results on
// the same side of any cut. These tests pin that invariant for the
// window-strategy prune (the only live off-boundary cut after the
// mid-turn position cutoff was removed) and lock the
// summarize-boundary cut head as a user_message by construction.

func heEntry(seq int64, m model.Message) HistoryEntry {
	return HistoryEntry{Seq: seq, Timestamp: time.Unix(seq, 0), Message: m}
}

func mUser(s string) model.Message  { return model.Message{Role: model.RoleUser, Content: s} }
func mAsstText(s string) model.Message {
	return model.Message{Role: model.RoleAssistant, Content: s}
}
func mAsstCalls(ids ...string) model.Message {
	tc := make([]model.ChunkToolCall, len(ids))
	for i, id := range ids {
		tc[i] = model.ChunkToolCall{ID: id, Name: "demo:do_thing"}
	}
	return model.Message{Role: model.RoleAssistant, ToolCalls: tc}
}
func mToolRes(id string) model.Message {
	return model.Message{Role: model.RoleTool, ToolCallID: id, Content: "result"}
}

func TestSnapToPairSafeHead(t *testing.T) {
	// [user, asst+3calls, tool, tool, tool, user, asst, user]
	entries := []HistoryEntry{
		heEntry(1, mUser("brief")),
		heEntry(2, mAsstCalls("t1", "t2", "t3")),
		heEntry(3, mToolRes("t1")),
		heEntry(4, mToolRes("t2")),
		heEntry(5, mToolRes("t3")),
		heEntry(6, mUser("u2")),
		heEntry(7, mAsstText("done")),
		heEntry(8, mUser("u3")),
	}
	cases := []struct {
		name string
		head int
		want int
	}{
		{"already clean (user)", 5, 5},
		{"already clean (assistant)", 6, 6},
		{"mid-group first result", 2, 1},  // entries[2] tool → back to owner @1
		{"mid-group middle result", 3, 1}, // walk over the whole run
		{"mid-group last result", 4, 1},
		{"empty tail head==len", len(entries), len(entries)}, // no panic, no pair
		{"head 0 stays", 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := snapToPairSafeHead(entries, tc.head); got != tc.want {
				t.Fatalf("snapToPairSafeHead(%d) = %d, want %d", tc.head, got, tc.want)
			}
		})
	}
}

func TestFirstUserMessageIndex(t *testing.T) {
	if got := firstUserMessageIndex([]HistoryEntry{
		heEntry(1, mAsstText("a")), heEntry(2, mUser("u")), heEntry(3, mUser("u2")),
	}); got != 1 {
		t.Fatalf("first user index = %d, want 1", got)
	}
	if got := firstUserMessageIndex([]HistoryEntry{heEntry(1, mAsstText("a"))}); got != -1 {
		t.Fatalf("no-user index = %d, want -1", got)
	}
}

// TestPruneWindow_PairSafeAndPinsBrief builds a history whose naive
// last-`limit` window would begin on an orphaned tool_result, and
// asserts the prune (a) never starts the window on a RoleTool, (b)
// keeps the whole N-parallel group intact, and (c) pins the first
// user_message (task brief) at index 0.
func TestPruneWindow_PairSafeAndPinsBrief(t *testing.T) {
	hist := []HistoryEntry{
		heEntry(1, mUser("brief")),               // 0 — the task brief
		heEntry(2, mAsstText("ack")),             // 1
		heEntry(3, mUser("u2")),                  // 2
		heEntry(4, mAsstCalls("t1", "t2", "t3")), // 3 — group owner
		heEntry(5, mToolRes("t1")),               // 4
		heEntry(6, mToolRes("t2")),               // 5
		heEntry(7, mToolRes("t3")),               // 6
		heEntry(8, mUser("u3")),                  // 7
		heEntry(9, mAsstText("more")),            // 8
		heEntry(10, mUser("u4")),                 // 9
	}
	s := &CompactorState{}
	s.resetHistory(append([]HistoryEntry(nil), hist...))

	// limit=5 → naive head = 10-(5-1) = 6 (a tool_result). Snap back
	// to the owner @3.
	s.pruneWindow(5)
	got := s.historySnapshot()

	if len(got) == 0 {
		t.Fatal("window pruned to empty")
	}
	// (c) brief pinned at index 0.
	if got[0].Seq != 1 || got[0].Message.Role != model.RoleUser {
		t.Fatalf("index 0 = seq %d role %q, want the brief (seq 1, user)", got[0].Seq, got[0].Message.Role)
	}
	// (a) the window portion must not begin on a tool_result. With the
	// brief pinned at 0, the window head is index 1.
	if got[1].Message.Role == model.RoleTool {
		t.Fatalf("window head (index 1) is a tool_result — pair split: %+v", got[1])
	}
	// (b) the group [asst+3calls, t1, t2, t3] survives contiguous.
	var ownerAt = -1
	for i, e := range got {
		if len(e.Message.ToolCalls) == 3 {
			ownerAt = i
			break
		}
	}
	if ownerAt < 0 {
		t.Fatalf("3-call assistant owner not preserved; got %d entries", len(got))
	}
	for k := 1; k <= 3; k++ {
		if got[ownerAt+k].Message.Role != model.RoleTool {
			t.Fatalf("result %d after owner is %q, want tool", k, got[ownerAt+k].Message.Role)
		}
	}
	// every tool_result kept must have its owner present.
	owners := map[string]bool{}
	for _, e := range got {
		for _, tc := range e.Message.ToolCalls {
			owners[tc.ID] = true
		}
	}
	for _, e := range got {
		if e.Message.Role == model.RoleTool && !owners[e.Message.ToolCallID] {
			t.Fatalf("orphaned tool_result %q (owner pruned)", e.Message.ToolCallID)
		}
	}
}

// TestPruneWindow_NoToolsKeepsLimit confirms the common no-tool case
// is unchanged in size: brief pinned + (limit-1) recent == limit.
func TestPruneWindow_NoToolsKeepsLimit(t *testing.T) {
	var hist []HistoryEntry
	for i := int64(1); i <= 20; i++ {
		if i%2 == 1 {
			hist = append(hist, heEntry(i, mUser("u")))
		} else {
			hist = append(hist, heEntry(i, mAsstText("a")))
		}
	}
	s := &CompactorState{}
	s.resetHistory(hist)
	s.pruneWindow(6)
	got := s.historySnapshot()
	if len(got) != 6 {
		t.Fatalf("no-tool window len = %d, want 6", len(got))
	}
	if got[0].Seq != 1 {
		t.Fatalf("brief not pinned: index 0 seq = %d, want 1", got[0].Seq)
	}
}

// TestShouldCompact_SingleTurnWorkerNeverCompacts locks the removed
// worker-mode bypass: a one-user_message session over MaxTokens does
// NOT trigger summarize compaction (it is handled by context-budget
// termination instead).
func TestShouldCompact_SingleTurnWorkerNeverCompacts(t *testing.T) {
	st := newFakeIntegrationState(t, "ses-worker")
	mdl := &stubModel{summary: "never"}
	router := newStubRouter(t, mdl)
	storeR := &fakeStoreReader{rows: fixtureRows(1, 1)}

	cfg := DefaultConfig()
	cfg.Strategy = StrategySummarize
	cfg.MaxTokens = 50
	cfg.PreservedRecentTurns = 10

	e := NewExtensionWithConfig(slog.Default(), cfg, Deps{Router: router, Store: storeR, AgentID: "a1"})
	ctx := context.Background()
	if err := e.InitState(ctx, st); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	// one user_message (BoundaryCount == 1) + a fat agent_message that
	// pushes the running estimate well past MaxTokens.
	um := protocol.NewUserMessage(st.SessionID(), protocol.ParticipantInfo{ID: "u1", Kind: protocol.ParticipantUser}, "task brief")
	um.SetSeq(1)
	e.OnFrameEmit(ctx, st, um)
	ag := protocol.NewAgentMessage(st.SessionID(), protocol.ParticipantInfo{ID: "a1", Kind: protocol.ParticipantAgent}, string(make([]byte, 4000)), 0, true)
	ag.Payload.Consolidated = true
	ag.SetSeq(2)
	e.OnFrameEmit(ctx, st, ag)

	s := FromState(st)
	if s.EstimatedPromptTokens() <= cfg.MaxTokens {
		t.Fatalf("precondition: estimate %d not over MaxTokens %d", s.EstimatedPromptTokens(), cfg.MaxTokens)
	}
	if e.shouldCompact(st, cfg) {
		t.Fatal("single-turn worker over budget must NOT trigger compaction")
	}
	if mdl.callCount() != 0 {
		t.Fatalf("summariser called %d times, want 0", mdl.callCount())
	}
}

// TestSummarizeBoundary_HeadIsUserMessage locks the summarize cut
// invariant for multi-turn (root/chat) sessions: even when each
// turn carries a tool_call → tool_result group, the preserved tail
// after compaction always BEGINS on a user_message (the cut lands on
// a turn boundary), so no tool_result is ever orphaned. Parameterized
// over the preserved-window parameter ("how much we keep from the
// bottom").
func TestSummarizeBoundary_HeadIsUserMessage(t *testing.T) {
	for _, preserved := range []int{0, 1, 3, 10} {
		preserved := preserved
		t.Run("preserved="+strconv.Itoa(preserved), func(t *testing.T) {
			st := newFakeIntegrationState(t, "ses-multi")
			mdl := &stubModel{summary: "summary of older turns"}
			router := newStubRouter(t, mdl)
			nTurns := preserved + 5
			storeR := &fakeStoreReader{rows: fixtureRows(nTurns, 1)}

			cfg := DefaultConfig()
			cfg.Strategy = StrategySummarize
			cfg.PreservedRecentTurns = preserved
			cfg.MaxTokens = 20 // token limb trips within the first few turns
			cfg.MaxTurns = 0
			cfg.MinTurnGap = 0
			cfg.DigestMaxTokens = 0

			e := NewExtensionWithConfig(slog.Default(), cfg, Deps{Router: router, Store: storeR, AgentID: "a1"})
			ctx := context.Background()
			if err := e.InitState(ctx, st); err != nil {
				t.Fatalf("InitState: %v", err)
			}

			// Each turn: user → assistant(1 tool_call) → tool_result →
			// final assistant. The tool group lives INSIDE the turn.
			auth := protocol.ParticipantInfo{ID: "a1", Kind: protocol.ParticipantAgent}
			usr := protocol.ParticipantInfo{ID: "u1", Kind: protocol.ParticipantUser}
			for turn := 0; turn < nTurns; turn++ {
				base := turn*4 + 1
				id := "tc" + strconv.Itoa(turn)
				um := protocol.NewUserMessage(st.SessionID(), usr, "user "+strconv.Itoa(turn))
				um.SetSeq(base)
				e.OnFrameEmit(ctx, st, um)
				ag := protocol.NewAgentMessageConsolidated(st.SessionID(), auth, "calling", base+1, false,
					[]protocol.ToolCallPayload{{ToolID: id, Name: "demo:do_thing"}}, "", "")
				ag.SetSeq(base + 1)
				e.OnFrameEmit(ctx, st, ag)
				tr := protocol.NewToolResult(st.SessionID(), auth, id, "result "+strconv.Itoa(turn), false)
				tr.SetSeq(base + 2)
				e.OnFrameEmit(ctx, st, tr)
				fin := protocol.NewAgentMessageConsolidated(st.SessionID(), auth, "done "+strconv.Itoa(turn), base+3, true, nil, "", "")
				fin.SetSeq(base + 3)
				e.OnFrameEmit(ctx, st, fin)
			}

			if err := e.OnTurnBoundary(ctx, st); err != nil {
				t.Fatalf("OnTurnBoundary: %v", err)
			}
			if got := countDigestSetFrames(st); got == 0 {
				t.Fatalf("expected a compaction to fire (preserved=%d, turns=%d)", preserved, nTurns)
			}

			owned := e.ProvideHistory(ctx, st)
			if len(owned) == 0 {
				t.Fatal("preserved tail empty after compaction")
			}
			if owned[0].Role != model.RoleUser {
				t.Fatalf("preserved tail head role = %q, want user_message (cut split a turn)", owned[0].Role)
			}
			// No orphaned tool_result anywhere in the preserved tail.
			owners := map[string]bool{}
			for _, m := range owned {
				for _, tc := range m.ToolCalls {
					owners[tc.ID] = true
				}
			}
			for _, m := range owned {
				if m.Role == model.RoleTool && !owners[m.ToolCallID] {
					t.Fatalf("orphaned tool_result %q in preserved tail", m.ToolCallID)
				}
			}
		})
	}
}
