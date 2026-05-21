package session

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

// materialise lazily reconstructs a Session's extension state from
// the persisted event log. Idempotent — second call is a no-op.
//
// η.3 — the model-visible history slice is rebuilt by the
// [extension.HistoryOwner] (compactor) via the same Recovery
// pass; the session itself owns no history projection.
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

		// Soft-warning idempotency derives from the event log so a
		// restart that loses in-memory state still skips re-emission.
		s.reloadSoftWarningFlag(rows)

		// Phase 5.2 (context-budget observability) — restore the
		// session's cumulative token-spend counter from the
		// latest persisted session_status row carrying Usage.
		// Walk events newest-last and stop at the first hit so
		// the cost is at most one O(n) scan; a session with no
		// recorded usage leaves cumulativeUsage at zero.
		s.restoreCumulativeUsage(rows)

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

// restoreCumulativeUsage walks events newest-last, finds the
// latest session_status row carrying a Usage block, and stamps
// the value onto Session.cumulativeUsage. Phase 5.2
// (context-budget observability).
//
// The Usage block rides session_status payloads (see
// SessionStatusPayload.Usage). The store flattens the typed
// payload into the row's Metadata column directly (see
// FrameToEventRow), so the usage entry is a map nested under
// the "usage" key. Re-marshal the map back into a typed payload
// to lift the counters out without dragging the codec into
// this package.
func (s *Session) restoreCumulativeUsage(rows []store.EventRow) {
	for i := len(rows) - 1; i >= 0; i-- {
		row := rows[i]
		if protocol.Kind(row.EventType) != protocol.KindSessionStatus {
			continue
		}
		raw, ok := row.Metadata["usage"]
		if !ok || raw == nil {
			continue
		}
		blob, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		var usage protocol.TokenUsage
		if err := json.Unmarshal(blob, &usage); err != nil {
			continue
		}
		s.restoreSessionUsage(usage)
		return
	}
}

