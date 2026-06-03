// history.go: η.1 — model-visible history projection. Mirrors
// the rules `pkg/session/replay.go::projectHistory` + the live
// turn-loop appenders apply today; the compactor maintains its
// own incremental copy via FrameObserver so a future flip of
// Session.buildMessages (η.2) reads from one owner instead of
// the parallel Session.history slice.
//
// η.1 ships the plumbing only — ProvideHistory is exposed, the
// projection is maintained in lockstep with emit, but
// Session.buildMessages stays on its legacy s.history slice.
// The byte-for-byte equivalence test in
// `history_projection_test.go` guards the swap planned for η.2.
package compactor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/prompts"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

// ProvideHistory implements [extension.HistoryOwner]. Returns a
// fresh copy of the projected entries as []model.Message — the
// caller may append to it without affecting owner-internal state.
//
// Stage 2 (L3) — when the model has hidden checkpoint-segments, the
// entries inside those segments are COLLAPSED into placeholders here:
// the full entries stay in [CompactorState.history] (lossless —
// context:expand flips them back), but ProvideHistory emits a tiny
// stand-in so the model's next prompt build sheds the tokens. The
// collapse preserves pair integrity (every assistant tool_call and
// every tool_result keeps its role + id; only the bodies shrink), so
// strict providers (Gemini) never see an orphaned function_response.
// When nothing is hidden the projection is the full, unmodified
// history — byte-for-byte the legacy slice.
func (e *Extension) ProvideHistory(_ context.Context, state extension.SessionState) []model.Message {
	s := FromState(state)
	if s == nil {
		return nil
	}
	entries := s.historySnapshot()
	if len(entries) == 0 {
		return nil
	}
	ranges := s.hiddenRanges()
	if len(ranges) == 0 {
		out := make([]model.Message, len(entries))
		for i, ent := range entries {
			out[i] = ent.Message
		}
		return out
	}
	out := make([]model.Message, 0, len(entries))
	noted := make(map[string]bool, len(ranges))
	for _, ent := range entries {
		r := matchHiddenRange(ranges, ent.Seq)
		if r == nil {
			out = append(out, ent.Message)
			continue
		}
		// First entry of this hidden segment carries the expand note as
		// a leading placeholder; the note appears exactly once per
		// hidden checkpoint regardless of how many entries it spans.
		if !noted[r.cp.ID] {
			noted[r.cp.ID] = true
			out = append(out, model.Message{
				Role:    model.RoleUser,
				Content: hiddenSegmentNote(r.cp),
			})
		}
		// Pair-integrity-bearing entries (assistant tool_calls, tool
		// results) stay as shrunk stubs that keep their role + ids;
		// everything else (plain assistant text, user / system filler)
		// is dropped — the note already stands in for it.
		if stub, keep := shrinkHiddenEntry(ent.Message); keep {
			out = append(out, stub)
		}
	}
	return out
}

// hiddenSegmentNote renders the one-line placeholder shown in place of
// a collapsed checkpoint-segment: what it was + how to bring it back.
func hiddenSegmentNote(cp Checkpoint) string {
	desc := strings.TrimSpace(cp.Description)
	if desc == "" {
		desc = "(no description)"
	}
	return fmt.Sprintf(
		"[context: checkpoint %s hidden — %s. ~%d tokens collapsed; call context:expand(cp_id=%q) to restore the detail]",
		cp.ID, desc, cp.Tokens, cp.ID)
}

// shrinkHiddenEntry returns the stub a hidden entry projects to, plus
// keep=true when the stub must be emitted to preserve pair integrity.
// keep=false means the entry is pure filler inside a hidden segment
// (plain assistant text, user / system message) and is dropped — the
// segment's leading note already represents it.
//
//   - assistant WITH tool_calls → keep role + each call's id + name,
//     blank the args (replayed history never re-validates args) and
//     clear content / reasoning. The following tool_result still finds
//     its matching call by id.
//   - tool result → keep role + tool_call_id, replace the (possibly
//     huge) body with a tiny marker.
//   - anything else → dropped.
func shrinkHiddenEntry(msg model.Message) (model.Message, bool) {
	switch msg.Role {
	case model.RoleAssistant:
		if len(msg.ToolCalls) == 0 {
			return model.Message{}, false
		}
		calls := make([]model.ChunkToolCall, len(msg.ToolCalls))
		for i, tc := range msg.ToolCalls {
			calls[i] = model.ChunkToolCall{
				ID:   tc.ID,
				Name: tc.Name,
				Args: map[string]any{},
			}
		}
		return model.Message{Role: model.RoleAssistant, ToolCalls: calls}, true
	case model.RoleTool:
		return model.Message{
			Role:       model.RoleTool,
			ToolCallID: msg.ToolCallID,
			Content:    "(hidden — context:expand to restore)",
		}, true
	default:
		return model.Message{}, false
	}
}

// RollbackTo implements [extension.HistoryOwner]. Delegates to
// the per-session state's RollbackTo so pkg/session can call
// through the interface without importing this package.
// Phase 5.2.η.3.
func (e *Extension) RollbackTo(_ context.Context, state extension.SessionState, seq int64) {
	s := FromState(state)
	if s == nil {
		return
	}
	s.RollbackTo(seq)
}

// projectFrameToEntry maps a live frame onto a [HistoryEntry],
// returning ok=false for kinds the model never sees. Mirrors the
// allow-list `pkg/session/visibility.go::projectFrameToHistory`
// applies for "[system: …]" prefixed frames plus the direct
// appenders in `pkg/session/turn.go` for assistant/tool turns.
//
// Renderer may be nil — only the SubagentStarted /
// SubagentResult branches need it, and the live emit path always
// has a renderer wired. nil callers short-circuit those two
// branches to ok=false (matches "fixture frames with no renderer
// can never produce that shape").
func projectFrameToEntry(renderer *prompts.Renderer, frame protocol.Frame) (HistoryEntry, bool) {
	if frame == nil {
		return HistoryEntry{}, false
	}
	seq := int64(frame.Seq())
	ts := frame.OccurredAt()
	switch v := frame.(type) {
	case *protocol.UserMessage:
		return HistoryEntry{
			Seq:       seq,
			Timestamp: ts,
			Message: model.Message{
				Role:    model.RoleUser,
				Content: v.Payload.Text,
			},
		}, true
	case *protocol.AgentMessage:
		// Streaming chunks (Consolidated=false) stay outbox-only and
		// never persist — they don't belong in history.
		if !v.Payload.Consolidated {
			return HistoryEntry{}, false
		}
		msg := model.Message{
			Role:             model.RoleAssistant,
			Content:          v.Payload.Text,
			Thinking:         v.Payload.Thinking,
			ThoughtSignature: v.Payload.ThoughtSignature,
		}
		if len(v.Payload.ToolCalls) > 0 {
			msg.ToolCalls = make([]model.ChunkToolCall, len(v.Payload.ToolCalls))
			for i, tc := range v.Payload.ToolCalls {
				msg.ToolCalls[i] = model.ChunkToolCall{
					ID:   tc.ToolID,
					Name: tc.Name,
					Args: tc.Args,
				}
			}
		}
		return HistoryEntry{Seq: seq, Timestamp: ts, Message: msg}, true
	case *protocol.ToolResult:
		body := stringifyToolResult(v.Payload.Result)
		return HistoryEntry{
			Seq:       seq,
			Timestamp: ts,
			Message: model.Message{
				Role:       model.RoleTool,
				ToolCallID: v.Payload.ToolID,
				Content:    body,
			},
		}, true
	case *protocol.SystemMessage:
		text := fmt.Sprintf("[system: %s] %s", v.Payload.Kind, v.Payload.Content)
		return HistoryEntry{
			Seq:       seq,
			Timestamp: ts,
			Message:   model.Message{Role: model.RoleUser, Content: text},
		}, true
	case *protocol.SubagentStarted:
		// Not rendered into model history. A spawn driven by a tool
		// call (recipe `task:*`, sync `spawn_mission`) blocks the
		// dispatcher, so this note lands BETWEEN the tool's
		// function_call and its function_response — strict providers
		// (Gemini) reject a function_response that doesn't immediately
		// follow its function_call. It is also redundant: the model
		// already learns the child from the spawn tool's result and
		// the outcome from the later subagent_result. Stays a live
		// observability frame for adapters / liveview.
		return HistoryEntry{}, false
	case *protocol.SubagentResult:
		if renderer == nil {
			return HistoryEntry{}, false
		}
		switch v.Payload.RenderMode {
		case protocol.SubagentRenderSilent:
			return HistoryEntry{}, false
		case protocol.SubagentRenderAsyncNotify:
			text := strings.TrimRight(renderer.MustRender(
				"interrupts/async_mission_completed",
				map[string]any{
					"MissionID":     v.Payload.SessionID,
					"Goal":          v.Payload.Goal,
					"Status":        statusFromReason(v.Payload.Reason),
					"Reason":        v.Payload.Reason,
					"ResultSummary": v.Payload.Result,
				},
			), "\n")
			return HistoryEntry{
				Seq:       seq,
				Timestamp: ts,
				Message:   model.Message{Role: model.RoleUser, Content: text},
			}, true
		}
		resBody := v.Payload.Result
		if resBody == "" {
			resBody = fmt.Sprintf("(no result; reason: %s)", v.Payload.Reason)
		}
		body := strings.TrimRight(renderer.MustRender(
			"system/subagent_result_render",
			map[string]any{
				"ChildID": v.Payload.SessionID,
				"Reason":  v.Payload.Reason,
				"Turns":   v.Payload.TurnsUsed,
				"Body":    resBody,
			},
		), "\n")
		text := "[system: subagent_result] " + body
		return HistoryEntry{
			Seq:       seq,
			Timestamp: ts,
			Message:   model.Message{Role: model.RoleUser, Content: text},
		}, true
	}
	return HistoryEntry{}, false
}

// projectRowToEntry mirrors [projectFrameToEntry] for the
// persisted-row replay path. Returns ok=false for kinds the
// model never sees and for digest-tracking ExtensionFrame rows
// (those are routed through the digest_set / digest_clear
// recovery branch instead).
//
// The implementation tracks `pkg/session/replay.go::projectHistory`
// case-for-case so a session that resumes through Recover sees
// the identical history slice the legacy materialise path would
// have built. Byte-for-byte equality is enforced by
// `history_projection_test.go`.
func projectRowToEntry(renderer *prompts.Renderer, row *store.EventRow) (HistoryEntry, bool) {
	if row == nil {
		return HistoryEntry{}, false
	}
	seq := int64(row.Seq)
	ts := row.CreatedAt
	switch protocol.Kind(row.EventType) {
	case protocol.KindUserMessage:
		return HistoryEntry{
			Seq:       seq,
			Timestamp: ts,
			Message:   model.Message{Role: model.RoleUser, Content: row.Content},
		}, true
	case protocol.KindAgentMessage:
		// Streaming-chunk rows (consolidated=false) are outbox-only
		// in current producers; older DBs may still carry them, so
		// keep the discriminator check.
		if cons, _ := metadataBool(row.Metadata, "consolidated"); !cons {
			return HistoryEntry{}, false
		}
		msg := model.Message{
			Role:             model.RoleAssistant,
			Content:          row.Content,
			ToolCalls:        decodeToolCallsFromMetadata(row.Metadata),
			Thinking:         metadataString(row.Metadata, "thinking"),
			ThoughtSignature: metadataString(row.Metadata, "thought_signature"),
		}
		return HistoryEntry{Seq: seq, Timestamp: ts, Message: msg}, true
	case protocol.KindToolResult:
		toolID := metadataString(row.Metadata, "tool_id")
		body := row.ToolResult
		if body == "" {
			body = row.Content
		}
		return HistoryEntry{
			Seq:       seq,
			Timestamp: ts,
			Message: model.Message{
				Role:       model.RoleTool,
				ToolCallID: toolID,
				Content:    body,
			},
		}, true
	case protocol.KindSystemMessage:
		kind, _ := row.Metadata["kind"].(string)
		if kind == "" {
			kind = "system"
		}
		return HistoryEntry{
			Seq:       seq,
			Timestamp: ts,
			Message: model.Message{
				Role:    model.RoleUser,
				Content: fmt.Sprintf("[system: %s] %s", kind, row.Content),
			},
		}, true
	case protocol.KindSubagentStarted:
		// Not rendered into model history — see the matching
		// *protocol.SubagentStarted case in projectFrameToEntry. A
		// tool-driven spawn (recipe `task:*`, sync `spawn_mission`)
		// emits this note while the dispatcher blocks, so it lands
		// between the tool's function_call and function_response and
		// breaks strict providers (Gemini); it is redundant with the
		// spawn tool's result + the later subagent_result anyway.
		return HistoryEntry{}, false
	case protocol.KindSubagentResult:
		if renderer == nil {
			return HistoryEntry{}, false
		}
		cid, _ := row.Metadata["session_id"].(string)
		reason, _ := row.Metadata["reason"].(string)
		renderMode, _ := row.Metadata["render_mode"].(string)
		turns := metadataInt(row.Metadata, "turns_used")
		body := row.Content
		if body == "" {
			if v, ok := row.Metadata["result"].(string); ok {
				body = v
			}
		}
		switch renderMode {
		case protocol.SubagentRenderSilent:
			return HistoryEntry{}, false
		case protocol.SubagentRenderAsyncNotify:
			goal, _ := row.Metadata["goal"].(string)
			rendered := strings.TrimRight(renderer.MustRender(
				"interrupts/async_mission_completed",
				map[string]any{
					"MissionID":     cid,
					"Goal":          goal,
					"Status":        statusFromReason(reason),
					"Reason":        reason,
					"ResultSummary": body,
				},
			), "\n")
			return HistoryEntry{
				Seq:       seq,
				Timestamp: ts,
				Message:   model.Message{Role: model.RoleUser, Content: rendered},
			}, true
		}
		if body == "" {
			body = fmt.Sprintf("(no result; reason: %s)", reason)
		}
		rendered := strings.TrimRight(renderer.MustRender(
			"system/subagent_result_render",
			map[string]any{
				"ChildID": cid,
				"Reason":  reason,
				"Turns":   turns,
				"Body":    body,
			},
		), "\n")
		return HistoryEntry{
			Seq:       seq,
			Timestamp: ts,
			Message:   model.Message{Role: model.RoleUser, Content: "[system: subagent_result] " + rendered},
		}, true
	}
	return HistoryEntry{}, false
}

// metadataBool / metadataString / metadataInt / metadataIntString
// mirror the helpers in pkg/session/replay.go. Local copies keep
// the package layering clean (session imports extension, not the
// reverse).
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

func metadataInt(m map[string]any, key string) int {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return 0
}

func metadataIntString(m map[string]any, key string) string {
	switch v := m[key].(type) {
	case float64:
		return fmt.Sprintf("%d", int(v))
	case int:
		return fmt.Sprintf("%d", v)
	case int64:
		return fmt.Sprintf("%d", v)
	}
	return ""
}

// decodeToolCallsFromMetadata lifts the persisted tool_calls
// array off a consolidated agent_message row. Mirrors the helper
// in pkg/session/replay.go.
func decodeToolCallsFromMetadata(m map[string]any) []model.ChunkToolCall {
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
		out = append(out, model.ChunkToolCall{
			ID:   metadataString(obj, "tool_id"),
			Name: metadataString(obj, "name"),
			Args: obj["args"],
		})
	}
	return out
}

// estimateMessageTokens sums [estimateTokens] over the visible
// surface of one model.Message: Content + Thinking +
// ToolCalls.Args (JSON-shape). Used by
// [CompactorState.HistoryTokens] to size the owned cache for
// the context-budget UI surface.
func estimateMessageTokens(msg model.Message) int {
	total := estimateTokens(msg.Content) + estimateTokens(msg.Thinking)
	for _, tc := range msg.ToolCalls {
		total += estimateTokens(tc.Name)
		total += estimateToolResultTokens(tc.Args)
	}
	return total
}

// statusFromReason mirrors the helper of the same name in
// `pkg/session/replay.go`. Kept local because pkg/session is
// downstream of pkg/extension/compactor and we can't import it.
// Identical to the session-side helper; if a future status code
// lands, update both in lockstep.
func statusFromReason(reason string) string {
	switch reason {
	case protocol.TerminationCompleted:
		return "completed"
	case protocol.TerminationHardCeiling:
		return "hard_ceiling"
	case protocol.TerminationCancelCascade:
		return "cancel_cascade"
	}
	if strings.HasPrefix(reason, protocol.TerminationSubagentCancelPrefix) {
		return "cancelled"
	}
	if strings.HasPrefix(reason, protocol.TerminationUserCancelPrefix) {
		return "cancelled"
	}
	if strings.HasPrefix(reason, protocol.TerminationPanicPrefix) {
		return "panic"
	}
	return reason
}

// stringifyToolResult mirrors the runtime's per-Kind handling
// for `result` payloads when projecting a ToolResult frame onto
// a RoleTool message. The persisted-row projection in
// `pkg/session/replay.go` reads `EventRow.ToolResult` (already
// a string) — here we render the same shape from the live
// payload's `any`.
func stringifyToolResult(v any) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
