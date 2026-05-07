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
// to the model; the model emits its own reasoning per turn. Tool
// calls are excluded too (Phase 3+ tools emit their own frames).
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
			// Only keep the final chunk per turn — partial deltas
			// aren't needed for replay. The "final" flag lives in
			// metadata; if missing we fall back to non-empty content.
			if final, _ := metadataBool(r.Metadata, "final"); final {
				all = append(all, model.Message{Role: model.RoleAssistant, Content: r.Content})
			}
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
