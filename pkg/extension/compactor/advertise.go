package compactor

import (
	"context"

	"github.com/hugr-lab/hugen/pkg/extension"
)

// AdvertiseSystemPrompt implements [extension.Advertiser]. When
// a digest is active, renders Block C — the chronological list
// of compacted SummaryBlocks + KeptVerbatim + SubagentRefs —
// via the agent-level template renderer.
//
// Phase α stub: returns empty string in all paths because no
// compaction fires yet (trigger predicate is short-circuited).
// β fills in the rendered output once SummaryBlocks start
// flowing.
//
// Block C placement in the system prompt: AFTER notepad
// Block A/B, BEFORE the conversation history. The runtime.Build
// wiring (pkg/runtime/extensions.go) controls the order by
// placing the compactor extension after notepad in the
// agent-level extension slice.
func (e *Extension) AdvertiseSystemPrompt(_ context.Context, state extension.SessionState) string {
	s := FromState(state)
	if s == nil {
		return ""
	}
	d := s.Digest()
	if d == nil {
		return ""
	}
	// Phase α: render path lands in β alongside the template.
	// For now, an active digest with no content surfaces
	// nothing — the model continues to see full history via
	// the existing path until β.
	return ""
}
