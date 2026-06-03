// checkpoints.go — Stage 2 (L3) in-turn context checkpoints. A
// checkpoint is a model-authored marker at the current history head;
// the range between two adjacent checkpoints (or the last checkpoint
// and the head) is a SEGMENT — the atomic unit for hide / rollback.
//
// This file owns the checkpoint state living on [CompactorState] (the
// fields are declared in state.go; the behaviour is here) plus the
// segment-token math the L3 triggers key off. The model-visible
// placeholder projection for hidden segments lives in history.go
// ([Extension.ProvideHistory]); the synthetic `context:*` tools that
// drive these methods live in context_provider.go; the per-iteration
// trigger evaluation lives in checkpoint_controller.go.
//
// Invariant (§6.2): the current (un-checkpointed) segment is always
// fully visible — hide / rollback operate only on CLOSED segments
// (those with a closing checkpoint). This is what makes the segment
// counter hide-immune (§6.3): hiding an older segment never changes
// the sum over entries with Seq > lastCheckpointSeq.
package compactor

import (
	"fmt"
	"sort"

	"github.com/hugr-lab/hugen/pkg/model"
)

// Checkpoint is one model-authored segment marker. ID is a stable
// "cp-N" minted monotonically across the session. Seq is the history
// head at stamp time — the segment it closes is (prev.Seq, Seq].
// Tokens is the estimated size of that segment when it was closed
// (a soft display + advisory figure). Hidden flips when the model
// calls context:hide / context:expand.
type Checkpoint struct {
	ID          string `json:"id"`
	Seq         int64  `json:"seq"`
	Description string `json:"description"`
	Tokens      int    `json:"tokens"`
	Hidden      bool   `json:"hidden"`
}

// hiddenRange is the [low, high] seq window of one hidden segment,
// carried with its checkpoint so the placeholder projection can emit
// the right expand note. low is EXCLUSIVE (the prior checkpoint's
// Seq), high is INCLUSIVE (this checkpoint's Seq): an entry belongs
// to the segment iff low < entry.Seq <= high.
type hiddenRange struct {
	low  int64
	high int64
	cp   Checkpoint
}

// LastCheckpointSeq returns the Seq of the most recent checkpoint, or
// 0 when none has been stamped. The current (open) segment is every
// cached entry with Seq greater than this.
func (s *CompactorState) LastCheckpointSeq() int64 {
	s.cpMu.Lock()
	defer s.cpMu.Unlock()
	return s.lastCheckpointSeq
}

// Checkpoints returns a copy of the ordered checkpoint list. Callers
// may mutate the returned slice freely.
func (s *CompactorState) Checkpoints() []Checkpoint {
	s.cpMu.Lock()
	defer s.cpMu.Unlock()
	if len(s.checkpoints) == 0 {
		return nil
	}
	out := make([]Checkpoint, len(s.checkpoints))
	copy(out, s.checkpoints)
	return out
}

// historyMaxSeq returns the largest Seq currently in the owned history
// cache, or 0 when empty. Used to stamp a checkpoint's head and to
// pin the in-flight rollback call during a rollback.
func (s *CompactorState) historyMaxSeq() int64 {
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	var max int64
	for _, ent := range s.history {
		if ent.Seq > max {
			max = ent.Seq
		}
	}
	return max
}

// SegmentTokens returns the estimated token size of the CURRENT (open)
// segment — Σ estimateMessageTokens over cached entries with Seq >
// max(lastCheckpointSeq, preambleFloor). Measured locally from the
// segment's own entries (not realPrompt − baseline) so it is
// hide-immune by the §6.2 invariant. The preamble floor keeps the task
// brief out of the first segment so the window measures only the
// model's work, not the (often multi-KB) handoff contract.
func (s *CompactorState) SegmentTokens() int {
	entries := s.historySnapshot()
	cutoff := s.LastCheckpointSeq()
	if f := s.preambleFloor(entries); f > cutoff {
		cutoff = f
	}
	total := 0
	for _, ent := range entries {
		if ent.Seq > cutoff {
			total += estimateMessageTokens(ent.Message)
		}
	}
	return total
}

// computePreambleFloor returns the seq of the last entry BEFORE the
// first tool-bearing assistant message — the boundary between the task
// preamble (brief + contract + system setup + pre-tool planning) and
// the model-generated work. Returns -1 when no tool call has happened
// yet (nothing is sheddable, so there is no floor to fix).
func computePreambleFloor(entries []HistoryEntry) int64 {
	for i := range entries {
		m := entries[i].Message
		if m.Role == model.RoleAssistant && len(m.ToolCalls) > 0 {
			if i == 0 {
				return 0
			}
			return entries[i-1].Seq
		}
	}
	return -1
}

// preambleFloor returns the cached preamble boundary, computing it once
// from the given entries when the first tool-bearing assistant first
// appears. Entries at or below it are never hidden and never counted
// toward the segment window. Returns 0 (no protection needed) until the
// first tool call establishes the boundary — there is nothing hideable
// before then.
func (s *CompactorState) preambleFloor(entries []HistoryEntry) int64 {
	s.cpMu.Lock()
	if s.preambleFloorSet {
		v := s.preambleFloorVal
		s.cpMu.Unlock()
		return v
	}
	s.cpMu.Unlock()

	f := computePreambleFloor(entries)
	if f < 0 {
		return 0
	}
	s.cpMu.Lock()
	if !s.preambleFloorSet {
		s.preambleFloorVal = f
		s.preambleFloorSet = true
	}
	v := s.preambleFloorVal
	s.cpMu.Unlock()
	return v
}

// AddCheckpoint stamps a checkpoint at the current history head with
// the supplied description, resets the segment counter (by advancing
// lastCheckpointSeq to the head), and returns the new checkpoint.
// Tokens records the size of the segment it just closed. The head and
// segment size are computed BEFORE taking cpMu so the lock is never
// held across the historyMu reads.
func (s *CompactorState) AddCheckpoint(description string) Checkpoint {
	head := s.historyMaxSeq()
	tokens := s.SegmentTokens()

	s.cpMu.Lock()
	defer s.cpMu.Unlock()
	s.cpCounter++
	cp := Checkpoint{
		ID:          fmt.Sprintf("cp-%d", s.cpCounter),
		Seq:         head,
		Description: description,
		Tokens:      tokens,
	}
	s.checkpoints = append(s.checkpoints, cp)
	s.lastCheckpointSeq = head
	return cp
}

// SetCheckpointHidden flips the Hidden flag on the checkpoint with the
// given id and returns the updated checkpoint + ok. ok=false when no
// checkpoint carries that id.
func (s *CompactorState) SetCheckpointHidden(id string, hidden bool) (Checkpoint, bool) {
	s.cpMu.Lock()
	defer s.cpMu.Unlock()
	for i := range s.checkpoints {
		if s.checkpoints[i].ID == id {
			s.checkpoints[i].Hidden = hidden
			return s.checkpoints[i], true
		}
	}
	return Checkpoint{}, false
}

// FindCheckpoint returns a copy of the checkpoint with the given id.
func (s *CompactorState) FindCheckpoint(id string) (Checkpoint, bool) {
	s.cpMu.Lock()
	defer s.cpMu.Unlock()
	for i := range s.checkpoints {
		if s.checkpoints[i].ID == id {
			return s.checkpoints[i], true
		}
	}
	return Checkpoint{}, false
}

// HiddenSegmentTokens sums the recorded Tokens of every currently-
// hidden checkpoint — i.e. roughly how much context the model has
// shed. Used by the advisory + the expand size-guard estimate.
func (s *CompactorState) HiddenSegmentTokens() int {
	s.cpMu.Lock()
	defer s.cpMu.Unlock()
	total := 0
	for i := range s.checkpoints {
		if s.checkpoints[i].Hidden {
			total += s.checkpoints[i].Tokens
		}
	}
	return total
}

// hiddenRanges returns the seq windows of every currently-hidden
// segment, sorted ascending by low. Each window is (prev.Seq,
// this.Seq] — built by walking the ordered checkpoint list so the
// low bound is the immediately-preceding checkpoint's Seq (0 for the
// first). Consumed by [Extension.ProvideHistory] to collapse hidden
// segments into placeholders.
func (s *CompactorState) hiddenRanges() []hiddenRange {
	s.cpMu.Lock()
	defer s.cpMu.Unlock()
	if len(s.checkpoints) == 0 {
		return nil
	}
	var out []hiddenRange
	var prevSeq int64
	for i := range s.checkpoints {
		cp := s.checkpoints[i]
		if cp.Hidden {
			out = append(out, hiddenRange{low: prevSeq, high: cp.Seq, cp: cp})
		}
		prevSeq = cp.Seq
	}
	sort.Slice(out, func(i, j int) bool { return out[i].low < out[j].low })
	return out
}

// matchHiddenRange returns the hidden range an entry falls into (low <
// seq <= high), or nil. ranges must be sorted ascending by low (as
// [hiddenRanges] returns them).
func matchHiddenRange(ranges []hiddenRange, seq int64) *hiddenRange {
	for i := range ranges {
		if seq > ranges[i].low && seq <= ranges[i].high {
			return &ranges[i]
		}
	}
	return nil
}

// rollbackFrom drops history entries strictly between cp.Seq and the
// current head (the in-flight context:rollback call), and removes
// every checkpoint with Seq > cp.Seq. The rolled-back-to checkpoint
// becomes the new head (lastCheckpointSeq = cp.Seq); the rollback
// call itself (the max-seq entry) is preserved so its about-to-be-
// emitted tool_result is not orphaned (pair integrity). Returns the
// number of entries dropped and ok=false when the id is unknown.
//
// Destructive by design (§6.5): for when the work after a checkpoint
// went wrong. Unlike hide it is not reversible — the dropped entries
// leave the owned cache.
func (s *CompactorState) rollbackFrom(cpID string) (dropped int, ok bool) {
	cp, found := s.FindCheckpoint(cpID)
	if !found {
		return 0, false
	}
	keepHead := s.historyMaxSeq()

	// Drop history entries with cp.Seq < Seq < keepHead. Keep ≤ cp.Seq
	// (the restored prefix) and ≥ keepHead (the rollback call itself).
	s.historyMu.Lock()
	kept := s.history[:0]
	for _, ent := range s.history {
		if ent.Seq <= cp.Seq || ent.Seq >= keepHead {
			kept = append(kept, ent)
		} else {
			dropped++
		}
	}
	out := make([]HistoryEntry, len(kept))
	copy(out, kept)
	s.history = out
	s.historyMu.Unlock()

	// Drop checkpoints after the restore point; it becomes the head.
	s.cpMu.Lock()
	var survivors []Checkpoint
	for _, c := range s.checkpoints {
		if c.Seq <= cp.Seq {
			survivors = append(survivors, c)
		}
	}
	s.checkpoints = survivors
	s.lastCheckpointSeq = cp.Seq
	s.cpMu.Unlock()

	return dropped, true
}
