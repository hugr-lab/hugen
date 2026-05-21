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
// Block C placement in the system prompt: AFTER notepad
// Block A/B, BEFORE the conversation history. The runtime.Build
// wiring (pkg/runtime/extensions.go) controls the order by
// placing the compactor extension after notepad in the
// agent-level extension slice.
//
// Returns "" on every "no content" path (no state, no digest,
// empty digest, no renderer, render failure) so the agent's
// system-prompt assembly silently skips compactor when there's
// nothing useful to render.
func (e *Extension) AdvertiseSystemPrompt(_ context.Context, state extension.SessionState) string {
	s := FromState(state)
	if s == nil {
		return ""
	}
	d := s.Digest()
	if d == nil {
		return ""
	}
	if len(d.SummaryBlocks) == 0 && len(d.KeptVerbatim) == 0 {
		return ""
	}
	if state.Prompts() == nil {
		return ""
	}
	out, err := state.Prompts().Render("compactor/block_c", d)
	if err != nil {
		e.logger.Warn("compactor: render block_c failed",
			"session", state.SessionID(), "err", err)
		return ""
	}
	return out
}
