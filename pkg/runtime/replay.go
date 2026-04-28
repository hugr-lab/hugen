package runtime

import (
	"context"
	"fmt"

	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// defaultHistoryWindow is the number of most-recent events the
// runtime feeds into the next model call after a resume. Phase 1's
// Compactor lands in phase 5; until then we cap at K most-recent
// user/agent messages.
const defaultHistoryWindow = 50

// materialise lazily reconstructs a Session's working window of
// model.Messages from session_events. Idempotent — second call is
// a no-op.
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
		s.materialised.Store(true)
	})
	return firstErr
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
