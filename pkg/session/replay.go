package session

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/plan"
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
//   - plan: full plan_op replay through pkg/session/plan.Project.
//     The plan survives history truncation — its source is the
//     full event log, not the windowed history.
func (s *Session) materialise(ctx context.Context) error {
	if s.materialised.Load() {
		return nil
	}
	var firstErr error
	s.matOnce.Do(func() {
		rows, err := s.store.ListEvents(ctx, s.id, ListEventsOpts{})
		if err != nil {
			firstErr = fmt.Errorf("session %s: list events: %w", s.id, err)
			return
		}
		s.history = projectHistory(rows, defaultHistoryWindow)

		s.planMu.Lock()
		s.plan = plan.Project(planEventsFrom(rows))
		s.planMu.Unlock()

		s.materialised.Store(true)
	})
	return firstErr
}

// planEventsFrom selects plan_op rows from the session's full event
// list and converts each into a plan.ProjectEvent. The session
// package owns this conversion so pkg/session/plan stays free of
// EventRow / store imports.
func planEventsFrom(rows []EventRow) []plan.ProjectEvent {
	out := make([]plan.ProjectEvent, 0)
	for _, r := range rows {
		if protocol.Kind(r.EventType) != protocol.KindPlanOp {
			continue
		}
		ev := plan.ProjectEvent{At: r.CreatedAt}
		// Payload travels two ways: a structured PlanOpPayload stashed
		// directly in Metadata (newer rows) or just Content carrying
		// the body (older / minimal rows). Try the structured shape
		// first; fall back to columnar fields.
		if r.Metadata != nil {
			if v, ok := r.Metadata["op"].(string); ok {
				ev.Op = v
			}
			if v, ok := r.Metadata["text"].(string); ok {
				ev.Text = v
			}
			if v, ok := r.Metadata["current_step"].(string); ok {
				ev.CurrentStep = v
			}
		}
		if ev.Op == "" {
			// Defensive fallback: serialised payload may have used a
			// `payload` envelope rather than top-level keys.
			if raw, ok := r.Metadata["payload"]; ok {
				b, _ := json.Marshal(raw)
				var p protocol.PlanOpPayload
				if json.Unmarshal(b, &p) == nil {
					ev.Op = p.Op
					ev.Text = p.Text
					ev.CurrentStep = p.CurrentStep
				}
			}
		}
		if ev.Text == "" {
			ev.Text = r.Content
		}
		if ev.Op == "" {
			continue
		}
		out = append(out, ev)
	}
	return out
}

// projectHistory walks events newest-last and keeps the most recent
// `window` user/agent text messages, rebuilding model.Message slice.
//
// Reasoning frames are excluded — phase 1 doesn't replay reasoning
// to the model; the model emits its own reasoning per turn. Tool
// calls are excluded too (Phase 3+ tools emit their own frames).
func projectHistory(rows []EventRow, window int) []model.Message {
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
