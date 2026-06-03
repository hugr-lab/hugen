package compactor

import (
	"context"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/model"
)

// stateWithCheckpoints wires a CompactorState onto a fakeState under
// StateKey so ProvideHistory / the context provider resolve it.
func stateWithCheckpoints(id string) (*fakeState, *CompactorState) {
	st := newFakeState(id)
	cs := &CompactorState{}
	st.SetValue(StateKey, cs)
	return st, cs
}

// TestProvideHistory_NoHiddenIsIdentity pins that the projection is the
// untouched history when nothing is hidden (the common case must not
// regress).
func TestProvideHistory_NoHiddenIsIdentity(t *testing.T) {
	ext := newTestExtension(t)
	st, cs := stateWithCheckpoints("ses-id")
	appendEntry(cs, 1, model.RoleUser, "task")
	appendToolPair(cs, 2, "call-1", "read_file", "big body A")
	cs.AddCheckpoint("read A")
	appendToolPair(cs, 4, "call-2", "grep", "big body B")

	out := ext.ProvideHistory(context.Background(), st)
	if len(out) != 5 {
		t.Fatalf("projection len = %d, want 5 (no collapse)", len(out))
	}
	if out[2].Content != "big body A" || out[4].Content != "big body B" {
		t.Fatalf("tool bodies altered without a hide: %+v", out)
	}
}

// TestProvideHistory_HiddenSegmentCollapses pins the load-bearing
// behaviour: a hidden segment collapses to a note + pair-integrity
// stubs (assistant tool_call ids + tool_result ids preserved, bodies
// shrunk); the task brief (preamble) stays visible even though it falls
// in cp-1's nominal range; entries outside the segment are untouched.
func TestProvideHistory_HiddenSegmentCollapses(t *testing.T) {
	ext := newTestExtension(t)
	st, cs := stateWithCheckpoints("ses-hide")
	appendEntry(cs, 1, model.RoleUser, "find the KPI")            // BRIEF — preamble, must stay
	appendToolPair(cs, 2, "call-1", "read_file", bigContent(800)) // first tool work → floor=1
	cs.AddCheckpoint("read the report")                           // cp-1 @ seq3, range (0,3]
	appendToolPair(cs, 4, "call-2", "edit_file", "ok")            // visible, after the checkpoint

	cs.SetCheckpointHidden("cp-1", true, "")
	out := ext.ProvideHistory(context.Background(), st)

	// Expected: [brief(visible)] [note] [assistant stub call-1]
	//           [tool stub call-1] [assistant call-2] [tool call-2]
	if len(out) != 6 {
		t.Fatalf("collapsed projection len = %d, want 6; got %+v", len(out), out)
	}
	// Preamble brief survives the hide (the whole point of the floor).
	if out[0].Role != model.RoleUser || out[0].Content != "find the KPI" {
		t.Fatalf("task brief must stay visible across a hide; got %+v", out[0])
	}
	if out[1].Role != model.RoleUser || !strings.Contains(out[1].Content, "cp-1") ||
		!strings.Contains(out[1].Content, "context:expand") {
		t.Fatalf("entry after the brief should be the expand note; got %+v", out[1])
	}
	// Pair-integrity stubs: assistant keeps the call id + name, args blanked.
	if out[2].Role != model.RoleAssistant || len(out[2].ToolCalls) != 1 ||
		out[2].ToolCalls[0].ID != "call-1" || out[2].ToolCalls[0].Name != "read_file" {
		t.Fatalf("hidden assistant stub lost its tool_call identity: %+v", out[2])
	}
	if out[3].Role != model.RoleTool || out[3].ToolCallID != "call-1" ||
		strings.Contains(out[3].Content, "xxxx") {
		t.Fatalf("hidden tool stub should keep id + shrink body: %+v", out[3])
	}
	// Visible pair untouched.
	if out[4].ToolCalls[0].ID != "call-2" || out[5].Content != "ok" {
		t.Fatalf("visible segment altered: %+v %+v", out[4], out[5])
	}

	// Pair integrity invariant: every tool_result id has a matching
	// assistant tool_call id earlier in the projection.
	calls := map[string]bool{}
	for _, m := range out {
		for _, tc := range m.ToolCalls {
			calls[tc.ID] = true
		}
		if m.Role == model.RoleTool && !calls[m.ToolCallID] {
			t.Fatalf("orphaned tool_result %q — no preceding tool_call (broke pair integrity)", m.ToolCallID)
		}
	}
}

// TestProvideHistory_MiddleHideKeepsPairAcrossBoundary pins the B42 fix:
// when a NON-oldest segment is hidden while an earlier segment stays
// VISIBLE, the segment note (a RoleUser message) must NOT land between
// the prior checkpoint's assistant tool_call (visible, end of segment A)
// and its tool_result (the hidden segment B's first entry) — strict APIs
// (Anthropic/OpenAI) reject the orphaned pair. The note must defer past
// the leading backward-pairing tool_result.
func TestProvideHistory_MiddleHideKeepsPairAcrossBoundary(t *testing.T) {
	ext := newTestExtension(t)
	st, cs := stateWithCheckpoints("ses-mid")

	asstCall := func(seq int64, id, name string) {
		cs.appendHistory(HistoryEntry{Seq: seq, Message: model.Message{
			Role:      model.RoleAssistant,
			ToolCalls: []model.ChunkToolCall{{ID: id, Name: name, Args: map[string]any{"k": "v"}}},
		}})
	}
	toolRes := func(seq int64, id, body string) {
		cs.appendHistory(HistoryEntry{Seq: seq, Message: model.Message{
			Role: model.RoleTool, ToolCallID: id, Content: body,
		}})
	}

	appendEntry(cs, 1, model.RoleUser, "task")           // brief, ≤ floor
	appendToolPair(cs, 2, "c1", "read", bigContent(400)) // seq2 asst + seq3 tool → floor=1
	asstCall(4, "cpA", "context:checkpoint")             // closing call of segment A
	cs.AddCheckpoint("A")                                // cp-1 @ seq4, range (0,4] — stays visible
	toolRes(5, "cpA", "checkpoint ok")                   // FIRST entry of seg B — pairs back to seq4
	appendToolPair(cs, 6, "c3", "grep", bigContent(400)) // seq6 asst + seq7 tool
	asstCall(8, "cpB", "context:checkpoint")
	cs.AddCheckpoint("B") // cp-2 @ seq8, range (4,8]

	cs.SetCheckpointHidden("cp-2", true, "") // hide the LATER segment; segment A stays visible
	out := ext.ProvideHistory(context.Background(), st)

	// Global pair integrity: every tool_result has a preceding tool_call.
	calls := map[string]bool{}
	for _, m := range out {
		for _, tc := range m.ToolCalls {
			calls[tc.ID] = true
		}
		if m.Role == model.RoleTool && !calls[m.ToolCallID] {
			t.Fatalf("orphaned tool_result %q (broke pair integrity): %+v", m.ToolCallID, out)
		}
	}

	// The cpA tool_result must IMMEDIATELY follow its assistant tool_call —
	// nothing (especially the RoleUser note) inserted between them.
	asstIdx := -1
	for i, m := range out {
		for _, tc := range m.ToolCalls {
			if tc.ID == "cpA" {
				asstIdx = i
			}
		}
	}
	if asstIdx < 0 || asstIdx+1 >= len(out) {
		t.Fatalf("cpA assistant tool_call missing from projection: %+v", out)
	}
	if next := out[asstIdx+1]; next.Role != model.RoleTool || next.ToolCallID != "cpA" {
		t.Fatalf("note (or other msg) split the cpA pair; entry after the tool_call = %+v\nfull: %+v", next, out)
	}

	// The hidden segment's note still appears exactly once (just later).
	notes := 0
	for _, m := range out {
		if m.Role == model.RoleUser && strings.Contains(m.Content, "cp-2") &&
			strings.Contains(m.Content, "context:expand") {
			notes++
		}
	}
	if notes != 1 {
		t.Fatalf("hidden cp-2 note count = %d, want exactly 1: %+v", notes, out)
	}
}

// TestPreambleFloor_ProtectsBriefAndSegmentCount pins the dogfood fix:
// the task preamble (everything up to the first tool call) is never
// hidden and never counted toward the segment window — only model-
// generated work is sheddable.
func TestPreambleFloor_ProtectsBriefAndSegmentCount(t *testing.T) {
	st, cs := stateWithCheckpoints("ses-pre")
	// Preamble: system + brief (heavy contract) + the model's pre-tool
	// planning. Then the first tool call.
	appendEntry(cs, 1, model.RoleUser, "[system] setup")
	appendEntry(cs, 2, model.RoleUser, bigContent(3000)) // heavy brief/contract
	appendEntry(cs, 3, model.RoleAssistant, "plan: I'll read the data")
	appendToolPair(cs, 4, "c1", "read_file", bigContent(2000)) // first tool work → floor=3

	// Segment counter must exclude the ~3K brief — only the ~2K of work
	// after the first tool call counts.
	seg := cs.SegmentTokens()
	if seg < 1500 || seg > 2800 {
		t.Fatalf("segment tokens = %d, want ≈2000 (brief excluded by floor)", seg)
	}

	// Hiding the segment must NOT collapse the brief/setup/plan.
	cs.AddCheckpoint("read data") // cp-1 @ seq5
	cs.SetCheckpointHidden("cp-1", true, "")
	ext := newTestExtension(t)
	out := ext.ProvideHistory(context.Background(), st)
	for _, m := range out {
		if m.Content == "[system] setup" || m.Role == model.RoleAssistant && m.Content == "plan: I'll read the data" {
			goto found
		}
	}
	t.Fatalf("preamble (system setup / plan) was collapsed by hide; projection: %+v", out)
found:
	// The heavy brief (3K user message) must also survive verbatim.
	for _, m := range out {
		if m.Role == model.RoleUser && len(m.Content) > 2000 {
			return
		}
	}
	t.Fatalf("heavy brief was not preserved across the hide")
}
