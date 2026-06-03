package compactor

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/model"
)

// subagentState is a fakeState that reports a non-root tier so the
// checkpoint controller arms (root-off tier gate).
type subagentState struct {
	*fakeState
	depth int
}

func (s *subagentState) Depth() int   { return s.depth }
func (s *subagentState) Tier() string { return "worker" }

func newSubagentState(id string, depth int) (*subagentState, *CompactorState) {
	base := newFakeState(id)
	cs := &CompactorState{}
	base.SetValue(StateKey, cs)
	return &subagentState{fakeState: base, depth: depth}, cs
}

// overWindow fills the current segment past the default 10K window.
func overWindow(cs *CompactorState) {
	appendEntry(cs, 1, model.RoleAssistant, "ask")
	cs.appendHistory(HistoryEntry{Seq: 2, Message: model.Message{
		Role: model.RoleTool, ToolCallID: "c1", Content: bigContent(11_000),
	}})
}

func TestEvaluateContext_RootIsInert(t *testing.T) {
	ext := newTestExtension(t)
	st := newFakeState("ses-root") // Depth()==0
	cs := &CompactorState{}
	st.SetValue(StateKey, cs)
	overWindow(cs)

	dec := ext.EvaluateContext(context.Background(), st, extension.ContextInput{
		RealPromptTokens: 99_000, Budget: 100_000,
	})
	if dec.CheckpointRequired || dec.ContextFull || dec.Inject != "" {
		t.Fatalf("root session must be inert; got %+v", dec)
	}
}

func TestEvaluateContext_SegmentWindowBlocks(t *testing.T) {
	ext := newTestExtension(t)
	st, cs := newSubagentState("ses-seg", 1)
	overWindow(cs)

	dec := ext.EvaluateContext(context.Background(), st, extension.ContextInput{})
	if !dec.CheckpointRequired {
		t.Fatalf("over-window segment must set CheckpointRequired; got %+v", dec)
	}
	if dec.ContextFull {
		t.Fatalf("no budget → ContextFull must stay false; got %+v", dec)
	}
	if !strings.Contains(dec.Inject, "context:checkpoint") {
		t.Fatalf("checkpoint advisory missing the tool name: %q", dec.Inject)
	}

	// After a checkpoint closes the segment, the block clears.
	cs.AddCheckpoint("closed it")
	dec2 := ext.EvaluateContext(context.Background(), st, extension.ContextInput{})
	if dec2.CheckpointRequired {
		t.Fatalf("checkpoint must clear the block; got %+v", dec2)
	}
}

func TestEvaluateContext_BudgetBandBlocks(t *testing.T) {
	ext := newTestExtension(t)
	st, cs := newSubagentState("ses-band", 1)
	appendEntry(cs, 1, model.RoleAssistant, "small") // segment under window
	cs.AddCheckpoint("seg")                          // give the advisory a segment to list

	// 85K of a 100K budget is over the 0.80 band (80K).
	dec := ext.EvaluateContext(context.Background(), st, extension.ContextInput{
		RealPromptTokens: 85_000, Budget: 100_000,
	})
	if !dec.ContextFull {
		t.Fatalf("occupancy over the 0.80 band must set ContextFull; got %+v", dec)
	}
	if !strings.Contains(dec.Inject, "context:hide") {
		t.Fatalf("context_full advisory missing hide guidance: %q", dec.Inject)
	}

	// Under the band → clears.
	dec2 := ext.EvaluateContext(context.Background(), st, extension.ContextInput{
		RealPromptTokens: 70_000, Budget: 100_000,
	})
	if dec2.ContextFull {
		t.Fatalf("occupancy under the band must clear ContextFull; got %+v", dec2)
	}
}

func TestEvaluateContext_DisabledConfigInert(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CheckpointsEnabled = false
	ext := NewExtensionWithConfig(slog.Default(), cfg, Deps{})
	st, cs := newSubagentState("ses-off", 1)
	overWindow(cs)

	dec := ext.EvaluateContext(context.Background(), st, extension.ContextInput{
		RealPromptTokens: 99_000, Budget: 100_000,
	})
	if dec.CheckpointRequired || dec.ContextFull {
		t.Fatalf("disabled config must be inert; got %+v", dec)
	}
}
