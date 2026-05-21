package compactor

import (
	"context"
	"encoding/json"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// OnFrameEmit implements [extension.FrameObserver]. Maintains
// the per-session [boundaryTracker]: records user_message seqs
// (for cutoff alignment) + bumps the running token estimate
// (for the budget trigger limb).
//
// Non-blocking: the observer runs on the session's emit hot
// path; all work is in-memory increments. No I/O, no LLM, no
// channel sends.
//
// Spec reference: §3.2 (FrameObserver capability) + §3.5
// (boundary detection) + §4.2 (token-budget limb).
func (e *Extension) OnFrameEmit(_ context.Context, state extension.SessionState, frame protocol.Frame) {
	s := FromState(state)
	if s == nil {
		return
	}
	seq := int64(frame.Seq())
	switch f := frame.(type) {
	case *protocol.UserMessage:
		s.appendBoundary(seq, estimateTokens(f.Payload.Text))
	case *protocol.AgentMessage:
		// Streaming chunks (Consolidated=false) are outbox-only and
		// never persist — they don't contribute to the model's next
		// prompt budget. EVERY consolidated agent_message persists:
		// per-iteration consolidations (Final=false) ride into the
		// next iteration's prompt as assistant context, and the
		// final turn consolidation (Final=true) likewise. Both must
		// be counted toward the budget limb.
		if f.Payload.Consolidated {
			s.addTokens(estimateTokens(f.Payload.Text))
		}
	case *protocol.ToolResult:
		s.addTokens(estimateToolResultTokens(f.Payload.Result))
	default:
		// reasoning / tool_call / system_marker / heartbeat /
		// extension_frame are either outbox-only or do not
		// materially contribute to the model prompt budget.
		// β refines this list once the per-Kind dispatch
		// table lands in compactor.go.
	}

	// η.1 — project allow-listed frames into the owned history
	// cache so a future flip of Session.buildMessages (η.2) reads
	// from one source of truth. ProvideHistory exposes the slice;
	// it stays dormant until the read-path swaps.
	if entry, ok := projectFrameToEntry(state.Prompts(), frame); ok {
		s.appendHistory(entry)
	}
}

// estimateTokens is the cheap per-string heuristic the running
// token estimate uses. char/4 is the long-standing rule of
// thumb for English; for other scripts it under-estimates,
// which is fine — the trigger predicate uses the estimate as a
// floor for compaction, not for the model's actual budget.
//
// γ will swap in a tokeniser-per-model implementation behind
// this signature without callers needing to know.
func estimateTokens(s string) int {
	if s == "" {
		return 0
	}
	// Round up so even short messages contribute at least 1.
	return (len(s) + 3) / 4
}

// estimateToolResultTokens is a thin wrapper around
// [estimateTokens] for [protocol.ToolResultPayload.Result],
// which is `any` (JSON shape varies by tool). We serialise to
// JSON once and count the resulting byte length — far cheaper
// than reflect-walking the value tree, and the JSON shape is
// what the model ultimately sees in the tool-result message.
//
// json.Marshal failure (should not happen — tool results are
// already wire-shaped) falls back to 0 tokens; the budget limb
// is approximate by design.
func estimateToolResultTokens(v any) int {
	if v == nil {
		return 0
	}
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return estimateTokens(string(b))
}
