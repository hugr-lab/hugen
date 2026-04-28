package http

import (
	"context"
	"strconv"
	"strings"

	"github.com/hugr-lab/hugen/pkg/runtime"
)

// maxReplayLimit caps how many historical events a single
// Last-Event-ID replay can drag in. The reconnection cursor is
// supposed to be the last id the consumer received, so a large
// gap is unusual; capping prevents accidental DoS via a cursor of
// 0 against a session with millions of events.
const maxReplayLimit = 10_000

// ReplaySource is the consumer-side view of the runtime store the
// http adapter needs for reconnection replay. *runtime.RuntimeStoreLocal
// satisfies it via its existing ListEvents method; tests can satisfy
// it with a slice-backed fake.
type ReplaySource interface {
	ListEvents(ctx context.Context, sessionID string, opts runtime.ListEventsOpts) ([]runtime.EventRow, error)
}

// parseLastEventID interprets the SSE reconnection cursor per
// contracts/sse-wire-format.md §"Last-Event-ID semantics".
//
//   - Empty / absent → (0, false): live tail, no replay.
//   - Non-integer → (0, false).
//   - Negative integer → (0, false).
//   - Valid non-negative integer N → (N, true): replay seq > N.
//
// The boolean reports "ok to replay"; the int is the cursor. A
// caller that gets (n, true) but finds zero matching rows still
// degrades to live tail without an error.
func parseLastEventID(header string) (int, bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0, false
	}
	n, err := strconv.Atoi(header)
	if err != nil {
		return 0, false
	}
	if n < 0 {
		return 0, false
	}
	return n, true
}

// loadReplay queries the replay source for events with seq > minSeq,
// capped at maxReplayLimit. Returns nil on no match.
func loadReplay(ctx context.Context, source ReplaySource, sessionID string, minSeq int) ([]runtime.EventRow, error) {
	rows, err := source.ListEvents(ctx, sessionID, runtime.ListEventsOpts{
		MinSeq: minSeq,
		Limit:  maxReplayLimit,
	})
	if err != nil {
		return nil, err
	}
	return rows, nil
}
