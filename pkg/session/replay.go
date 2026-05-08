package session

import (
	"context"
	"fmt"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

// defaultHistoryWindow is the number of most-recent events the
// runtime feeds into the next model call after a resume. Phase 1's
// Compactor lands in phase 5; until then we cap at K most-recent
// user/agent messages.
const defaultHistoryWindow = 50

// materialise lazily reconstructs a Session's working window of
// model.Messages from session_events. Idempotent — second call is
// a no-op.
//
// Re-derived projections (phase-4):
//   - history: most-recent user/agent text messages within the
//     window cap (placeholder for phase-5 compactor).
//
// Extension-owned projections (plan, whiteboard, skill, …) ride
// the [extension.Recovery] hook below — every Recovery-implementing
// extension sees the same full event slice and rebuilds its own
// state.
func (s *Session) materialise(ctx context.Context) error {
	if s.materialised.Load() {
		return nil
	}
	var firstErr error
	s.matOnce.Do(func() {
		rows, err := s.store.ListEvents(ctx, s.id, store.ListEventsOpts{})
		if err != nil {
			firstErr = fmt.Errorf("session %s: list events: %w", s.id, err)
			return
		}
		s.history = projectHistory(rows, defaultHistoryWindow)

		// Soft-warning idempotency derives from the event log so a
		// restart that loses in-memory state still skips re-emission.
		s.reloadSoftWarningFlag(rows)

		// Extension recovery: every Recovery-implementing extension
		// rebuilds its per-session projection from the same event
		// list. Errors are logged warn-not-fatal — recovery is
		// best-effort and must not block session start. Order
		// follows registration order; an extension's recovery sees
		// the projections set up by InitState plus whatever earlier
		// recoveries wrote into state.
		if s.deps != nil {
			for _, ext := range s.deps.Extensions {
				rec, ok := ext.(extension.Recovery)
				if !ok {
					continue
				}
				if err := rec.Recover(ctx, s, rows); err != nil && s.deps.Logger != nil {
					s.deps.Logger.Warn("session: extension recovery failed",
						"session", s.id, "extension", ext.Name(), "err", err)
				}
			}
		}

		s.materialised.Store(true)
	})
	return firstErr
}

// projectHistory walks events newest-last and keeps the most recent
// `window` user/agent/system text messages, rebuilding model.Message
// slice.
//
// Reasoning frames are excluded — phase 1 doesn't replay reasoning
// to the model from per-chunk rows; the consolidated final
// agent_message carries the Anthropic/Gemini thinking + signature
// fields directly when set, so the chain continues across resume.
//
// AgentMessage rows are only consolidated finals (Final=true with
// full assembled text). Streaming chunks (Final=false) are
// outbox-only and never persisted — see emit() in session.go. So
// reading Content directly gives the full assistant text and lifting
// tool_calls / thinking from metadata gives a well-formed
// model.Message with no chunk reassembly.
//
// tool_result rows replay as RoleTool messages, paired with the
// tool_call id from the consolidated assistant turn that requested
// them. Standalone tool_call rows are skipped — the assistant
// message already lists them in ToolCalls.
//
// system_message rows ARE projected — as RoleUser with the same
// "[system: <kind>] <content>" prefix the live visibility filter
// uses (visibility.go projectFrameToHistory). Without this the
// runtime-injected nudges (soft_warning, stuck_nudge, whiteboard
// broadcasts, the auto-respawn spawned_note added by phase-4 US6)
// would be invisible to the model after a process restart.
// Reading the same shape live and after replay keeps the model's
// mental model continuous across the cut.
func projectHistory(rows []store.EventRow, window int) []model.Message {
	if window <= 0 {
		window = defaultHistoryWindow
	}
	// First, project relevant rows in original order.
	all := make([]model.Message, 0, len(rows))
	for _, r := range rows {
		switch protocol.Kind(r.EventType) {
		case protocol.KindUserMessage:
			all = append(all, model.Message{Role: model.RoleUser, Content: r.Content})
		case protocol.KindAgentMessage:
			// Persisted AgentMessage rows are per-iteration consolidated
			// records (see session.emit); streaming chunks are
			// outbox-only and never reach us here. Defensive: if a
			// pre-this-change DB has chunk rows mixed in, the absence
			// of "consolidated" metadata is the discriminator — skip
			// them so we don't replay partial deltas as turns.
			if cons, _ := metadataBool(r.Metadata, "consolidated"); !cons {
				continue
			}
			msg := model.Message{
				Role:             model.RoleAssistant,
				Content:          r.Content,
				ToolCalls:        decodeToolCalls(r.Metadata),
				Thinking:         metadataString(r.Metadata, "thinking"),
				ThoughtSignature: metadataString(r.Metadata, "thought_signature"),
			}
			all = append(all, msg)
		case protocol.KindToolResult:
			toolID := metadataString(r.Metadata, "tool_id")
			body := r.ToolResult
			if body == "" {
				body = r.Content
			}
			all = append(all, model.Message{
				Role:       model.RoleTool,
				ToolCallID: toolID,
				Content:    body,
			})
		case protocol.KindSystemMessage:
			kind, _ := r.Metadata["kind"].(string)
			if kind == "" {
				kind = "system"
			}
			all = append(all, model.Message{
				Role:    model.RoleUser,
				Content: fmt.Sprintf("[system: %s] %s", kind, r.Content),
			})
		case protocol.KindSubagentStarted:
			cid, _ := r.Metadata["child_session_id"].(string)
			role, _ := r.Metadata["role"].(string)
			depthStr := ""
			switch v := r.Metadata["depth"].(type) {
			case float64:
				depthStr = fmt.Sprintf("%d", int(v))
			case int:
				depthStr = fmt.Sprintf("%d", v)
			case int64:
				depthStr = fmt.Sprintf("%d", v)
			}
			all = append(all, model.Message{
				Role: model.RoleUser,
				Content: fmt.Sprintf("[system: %s] spawned %s (role: %s) at depth %s",
					protocol.SystemMessageSpawnedNote, cid, role, depthStr),
			})
		case protocol.KindSubagentResult:
			cid, _ := r.Metadata["session_id"].(string)
			reason, _ := r.Metadata["reason"].(string)
			turns := 0
			switch v := r.Metadata["turns_used"].(type) {
			case float64:
				turns = int(v)
			case int:
				turns = v
			case int64:
				turns = int(v)
			}
			body := r.Content
			if body == "" {
				if v, ok := r.Metadata["result"].(string); ok {
					body = v
				}
			}
			if body == "" {
				body = fmt.Sprintf("(no result; reason: %s)", reason)
			}
			all = append(all, model.Message{
				Role: model.RoleUser,
				Content: fmt.Sprintf("[system: subagent_result] %s reason=%s turns=%d\n%s",
					cid, reason, turns, body),
			})
		}
	}
	if len(all) <= window {
		return all
	}
	return all[len(all)-window:]
}

func metadataBool(m map[string]any, key string) (bool, bool) {
	if m == nil {
		return false, false
	}
	v, ok := m[key]
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

func metadataString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

// decodeToolCalls lifts the tool_calls array stashed in the
// consolidated final AgentMessage's metadata back into
// model.ChunkToolCall — the shape model.Message.ToolCalls expects so
// the model can resume its conversation knowing what it requested.
// Returns nil when absent or malformed.
func decodeToolCalls(m map[string]any) []model.ChunkToolCall {
	raw, ok := m["tool_calls"].([]any)
	if !ok || len(raw) == 0 {
		return nil
	}
	out := make([]model.ChunkToolCall, 0, len(raw))
	for _, e := range raw {
		obj, ok := e.(map[string]any)
		if !ok {
			continue
		}
		call := model.ChunkToolCall{
			ID:   metadataString(obj, "tool_id"),
			Name: metadataString(obj, "name"),
		}
		call.Args = obj["args"]
		out = append(out, call)
	}
	return out
}
