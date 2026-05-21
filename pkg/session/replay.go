package session

import (
	"context"
	"fmt"

	"github.com/hugr-lab/hugen/pkg/extension"
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

