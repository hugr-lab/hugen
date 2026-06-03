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
// shrunk), filler dropped, and entries outside the segment untouched.
func TestProvideHistory_HiddenSegmentCollapses(t *testing.T) {
	ext := newTestExtension(t)
	st, cs := stateWithCheckpoints("ses-hide")
	appendEntry(cs, 1, model.RoleUser, "find the KPI")            // filler in hidden range
	appendToolPair(cs, 2, "call-1", "read_file", bigContent(800)) // big pair in hidden range
	cs.AddCheckpoint("read the report")                           // cp-1 @ seq3, range (0,3]
	appendToolPair(cs, 4, "call-2", "edit_file", "ok")            // visible, after the checkpoint

	cs.SetCheckpointHidden("cp-1", true)
	out := ext.ProvideHistory(context.Background(), st)

	// Expected shape: [note(user)] [assistant stub call-1] [tool stub call-1]
	//                 [assistant call-2] [tool call-2]
	if len(out) != 5 {
		t.Fatalf("collapsed projection len = %d, want 5; got %+v", len(out), out)
	}
	if out[0].Role != model.RoleUser || !strings.Contains(out[0].Content, "cp-1") ||
		!strings.Contains(out[0].Content, "context:expand") {
		t.Fatalf("first entry should be the expand note; got %+v", out[0])
	}
	// Pair-integrity stubs: assistant keeps the call id + name, args blanked.
	if out[1].Role != model.RoleAssistant || len(out[1].ToolCalls) != 1 ||
		out[1].ToolCalls[0].ID != "call-1" || out[1].ToolCalls[0].Name != "read_file" {
		t.Fatalf("hidden assistant stub lost its tool_call identity: %+v", out[1])
	}
	if out[2].Role != model.RoleTool || out[2].ToolCallID != "call-1" ||
		strings.Contains(out[2].Content, "xxxx") {
		t.Fatalf("hidden tool stub should keep id + shrink body: %+v", out[2])
	}
	// Visible pair untouched.
	if out[3].ToolCalls[0].ID != "call-2" || out[4].Content != "ok" {
		t.Fatalf("visible segment altered: %+v %+v", out[3], out[4])
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
