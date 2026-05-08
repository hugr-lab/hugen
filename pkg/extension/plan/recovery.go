package plan

import (
	"context"
	"encoding/json"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

// Recover implements [extension.Recovery]. Walks plan_op events in
// arrival order and rebuilds the [SessionPlan] projection for the
// calling session by handing the materialised list to [Project].
//
// Best-effort: any decode failure on a single row is skipped — a
// row with no decodable op contributes nothing to the projection.
// The runtime logs the returned error as a warning; recovery never
// blocks session start.
func (e *Extension) Recover(_ context.Context, state extension.SessionState, events []store.EventRow) error {
	h := FromState(state)
	if h == nil {
		return nil
	}
	projected := Project(eventsFromRows(events))
	h.mu.Lock()
	h.plan = projected
	h.mu.Unlock()
	return nil
}

// eventsFromRows selects plan_op rows from the session's full event
// list and converts each into a [ProjectEvent]. Stays in the plan
// extension package so the core projection (plan_core.go) stays
// free of EventRow / store imports.
//
// Payload travels two ways: a structured PlanOpPayload stashed
// directly in Metadata (newer rows) or a `payload` envelope under
// Metadata (older / minimal rows). Defensively try the flat shape
// first, then fall back.
func eventsFromRows(rows []store.EventRow) []ProjectEvent {
	out := make([]ProjectEvent, 0)
	for _, r := range rows {
		if protocol.Kind(r.EventType) != protocol.KindPlanOp {
			continue
		}
		ev := ProjectEvent{At: r.CreatedAt}
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
