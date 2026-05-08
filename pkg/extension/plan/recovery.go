package plan

import (
	"context"
	"encoding/json"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

// Recover implements [extension.Recovery]. Walks plan extension_frame
// events in arrival order and rebuilds the [SessionPlan] projection
// for the calling session by handing the materialised list to
// [Project].
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

// eventsFromRows selects plan extension_frame rows from the session's
// full event list and converts each into a [ProjectEvent]. Stays in
// the plan extension package so the core projection (plan_core.go)
// stays free of EventRow / store imports.
//
// Row metadata is the flat ExtensionFramePayload {extension, category,
// op, data}. We filter on extension=="plan" + non-empty op, then
// decode the typed [OpData] from the `data` sub-object. Falls back
// to the row's Content column when text is missing — it carries the
// human-readable surface for plan set/comment ops (see
// pkg/session/store/store.go ExtensionFrame case).
func eventsFromRows(rows []store.EventRow) []ProjectEvent {
	out := make([]ProjectEvent, 0)
	for _, r := range rows {
		if protocol.Kind(r.EventType) != protocol.KindExtensionFrame {
			continue
		}
		if r.Metadata == nil {
			continue
		}
		if ext, _ := r.Metadata["extension"].(string); ext != providerName {
			continue
		}
		op, _ := r.Metadata["op"].(string)
		if op == "" {
			continue
		}
		ev := ProjectEvent{At: r.CreatedAt, Op: op}
		if raw, ok := r.Metadata["data"]; ok && raw != nil {
			b, _ := json.Marshal(raw)
			var d OpData
			if json.Unmarshal(b, &d) == nil {
				ev.Text = d.Text
				ev.CurrentStep = d.CurrentStep
			}
		}
		if ev.Text == "" {
			ev.Text = r.Content
		}
		out = append(out, ev)
	}
	return out
}
