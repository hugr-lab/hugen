package compactor

import (
	"testing"

	"github.com/hugr-lab/hugen/pkg/model"
)

// appendEntry is a test helper: appends one projected history entry of
// the given seq + role + content. Content length drives the token
// estimate (estimateMessageTokens ≈ len/4), so callers size content to
// hit a target segment weight.
func appendEntry(s *CompactorState, seq int64, role, content string) {
	s.appendHistory(HistoryEntry{Seq: seq, Message: model.Message{Role: role, Content: content}})
}

// appendToolPair appends an assistant tool_call (seq) + its tool_result
// (seq+1), the pair-integrity unit hide must preserve.
func appendToolPair(s *CompactorState, seq int64, callID, name, resultBody string) {
	s.appendHistory(HistoryEntry{Seq: seq, Message: model.Message{
		Role:      model.RoleAssistant,
		ToolCalls: []model.ChunkToolCall{{ID: callID, Name: name, Args: map[string]any{"k": "v"}}},
	}})
	s.appendHistory(HistoryEntry{Seq: seq + 1, Message: model.Message{
		Role:       model.RoleTool,
		ToolCallID: callID,
		Content:    resultBody,
	}})
}

func bigContent(approxTokens int) string {
	b := make([]byte, approxTokens*4)
	for i := range b {
		b[i] = 'x'
	}
	return string(b)
}

func TestAddCheckpoint_ResetsSegmentAndStamps(t *testing.T) {
	s := &CompactorState{}
	appendEntry(s, 1, model.RoleUser, bigContent(100))
	appendEntry(s, 2, model.RoleAssistant, bigContent(200))
	appendEntry(s, 3, model.RoleTool, bigContent(300))

	if got := s.SegmentTokens(); got < 500 {
		t.Fatalf("segment tokens before checkpoint = %d, want ≈600", got)
	}
	cp := s.AddCheckpoint("first segment")
	if cp.ID != "cp-1" {
		t.Fatalf("first checkpoint id = %q, want cp-1", cp.ID)
	}
	if cp.Seq != 3 {
		t.Fatalf("checkpoint stamped at seq %d, want 3 (history head)", cp.Seq)
	}
	if cp.Tokens < 500 {
		t.Fatalf("checkpoint recorded tokens = %d, want the closed segment size ≈600", cp.Tokens)
	}
	if got := s.SegmentTokens(); got != 0 {
		t.Fatalf("segment tokens after checkpoint = %d, want 0 (counter reset)", got)
	}
	// New work lands in a fresh segment, measured from the checkpoint.
	appendEntry(s, 4, model.RoleAssistant, bigContent(150))
	if got := s.SegmentTokens(); got < 100 || got > 250 {
		t.Fatalf("new-segment tokens = %d, want ≈150", got)
	}
}

func TestSegmentTokens_HideImmune(t *testing.T) {
	s := &CompactorState{}
	appendEntry(s, 1, model.RoleAssistant, bigContent(400))
	appendEntry(s, 2, model.RoleTool, bigContent(400))
	s.AddCheckpoint("closed") // cp-1 @ seq2
	appendEntry(s, 3, model.RoleAssistant, bigContent(120))
	appendEntry(s, 4, model.RoleTool, bigContent(120))

	before := s.SegmentTokens()
	if _, ok := s.SetCheckpointHidden("cp-1", true); !ok {
		t.Fatalf("hide cp-1 failed")
	}
	after := s.SegmentTokens()
	if before != after {
		t.Fatalf("segment tokens changed on hide: before=%d after=%d (must be hide-immune)", before, after)
	}
}

func TestSetCheckpointHidden_ToggleAndMissing(t *testing.T) {
	s := &CompactorState{}
	appendEntry(s, 1, model.RoleTool, bigContent(10))
	s.AddCheckpoint("a")
	if cp, ok := s.SetCheckpointHidden("cp-1", true); !ok || !cp.Hidden {
		t.Fatalf("hide cp-1: ok=%v hidden=%v", ok, cp.Hidden)
	}
	if cp, ok := s.SetCheckpointHidden("cp-1", false); !ok || cp.Hidden {
		t.Fatalf("expand cp-1: ok=%v hidden=%v", ok, cp.Hidden)
	}
	if _, ok := s.SetCheckpointHidden("cp-99", true); ok {
		t.Fatalf("hiding unknown checkpoint reported ok")
	}
}

func TestHiddenRanges(t *testing.T) {
	s := &CompactorState{}
	appendEntry(s, 1, model.RoleTool, bigContent(10))
	appendEntry(s, 2, model.RoleTool, bigContent(10))
	s.AddCheckpoint("seg1") // cp-1 @ seq2, range (0,2]
	appendEntry(s, 3, model.RoleTool, bigContent(10))
	appendEntry(s, 4, model.RoleTool, bigContent(10))
	s.AddCheckpoint("seg2") // cp-2 @ seq4, range (2,4]

	// Only cp-2 hidden → one range (2,4].
	s.SetCheckpointHidden("cp-2", true)
	ranges := s.hiddenRanges()
	if len(ranges) != 1 {
		t.Fatalf("hiddenRanges len = %d, want 1", len(ranges))
	}
	if ranges[0].low != 2 || ranges[0].high != 4 || ranges[0].cp.ID != "cp-2" {
		t.Fatalf("range = %+v, want low=2 high=4 cp-2", ranges[0])
	}
	if matchHiddenRange(ranges, 1) != nil {
		t.Fatalf("seq 1 should not match the (2,4] range")
	}
	if matchHiddenRange(ranges, 3) == nil {
		t.Fatalf("seq 3 should match the (2,4] range")
	}
	if matchHiddenRange(ranges, 4) == nil {
		t.Fatalf("seq 4 (inclusive high) should match")
	}
}

func TestRollbackFrom(t *testing.T) {
	s := &CompactorState{}
	appendEntry(s, 1, model.RoleUser, "task")
	appendEntry(s, 2, model.RoleTool, "a")
	s.AddCheckpoint("seg1") // cp-1 @ seq2
	appendEntry(s, 3, model.RoleAssistant, "work")
	appendEntry(s, 4, model.RoleTool, "b")
	s.AddCheckpoint("seg2") // cp-2 @ seq4
	// seq5 stands in for the in-flight context:rollback assistant call.
	appendEntry(s, 5, model.RoleAssistant, "rollback call")

	dropped, ok := s.rollbackFrom("cp-1")
	if !ok {
		t.Fatalf("rollbackFrom cp-1 not ok")
	}
	if dropped != 2 {
		t.Fatalf("dropped = %d, want 2 (seq 3 and 4)", dropped)
	}
	// History keeps ≤ cp.Seq (1,2) + the rollback call (5).
	got := s.historySnapshot()
	var seqs []int64
	for _, e := range got {
		seqs = append(seqs, e.Seq)
	}
	if len(seqs) != 3 || seqs[0] != 1 || seqs[1] != 2 || seqs[2] != 5 {
		t.Fatalf("post-rollback seqs = %v, want [1 2 5]", seqs)
	}
	// Checkpoints after the restore point are gone; cp-1 is the head.
	cps := s.Checkpoints()
	if len(cps) != 1 || cps[0].ID != "cp-1" {
		t.Fatalf("post-rollback checkpoints = %+v, want only cp-1", cps)
	}
	if s.LastCheckpointSeq() != 2 {
		t.Fatalf("lastCheckpointSeq = %d, want 2", s.LastCheckpointSeq())
	}
	if _, ok := s.rollbackFrom("cp-404"); ok {
		t.Fatalf("rollback of unknown checkpoint reported ok")
	}
}
