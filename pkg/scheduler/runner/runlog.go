package runner

import (
	"context"
	"sync"
	"time"
)

// RunLogStatus enumerates the lifecycle of a single fire's log
// row. The terminal value is recorded when the fn returns; an
// in_flight row that outlives the Runner (process crash) is
// observable as the dangling start-without-end.
type RunLogStatus string

const (
	RunLogInFlight  RunLogStatus = "in_flight"
	RunLogCompleted RunLogStatus = "completed"
	RunLogFailed    RunLogStatus = "failed"
	RunLogTimeout   RunLogStatus = "timeout"
)

// RunLogEntry is one fire's audit row. The fire is identified by
// (Name, FireSeq); the same pair is updated on completion via
// [RunnerRunLog.Finalize]. Lighter than a full TaskRunRow — no FK
// to a task row, no outcome blob; just enough to render an
// operator-visible "last N fires of <name>" listing.
type RunLogEntry struct {
	Name         string
	FireSeq      int
	PlannedAt    time.Time
	StartedAt    time.Time
	CompletedAt  time.Time
	Status       RunLogStatus
	ErrorMessage string
	Summary      string
	Duration     time.Duration
}

// RunnerRunLog is the Runner's audit sink. The in-memory
// implementation [NewMemoryRunLog] is the Phase 6.1a default and
// suffices for resilience reapers; an optional persistent backing
// (DuckDB / Postgres) lands with Phase 6.1b for telemetry retention.
//
// Implementations MUST be safe for concurrent use — Append +
// Finalize fire from the per-fire goroutines while ListByName runs
// on operator query paths.
type RunnerRunLog interface {
	Append(ctx context.Context, entry RunLogEntry) error
	Finalize(ctx context.Context, name string, seq int, entry RunLogEntry) error
	ListByName(ctx context.Context, name string, limit int) ([]RunLogEntry, error)
	LatestSuccess(ctx context.Context, name string) (RunLogEntry, bool, error)
}

// memoryRunLog keeps a ring of the last entries per name in
// process memory. Default cap is 128 per name — large enough to
// surface "the last day of hourly reaper fires" without unbounded
// growth on long-lived runners.
type memoryRunLog struct {
	mu      sync.RWMutex
	capPer  int
	entries map[string][]RunLogEntry
}

// NewMemoryRunLog constructs the default in-memory log. capPer
// caps the per-name retention; pass <= 0 for the default of 128.
func NewMemoryRunLog(capPer int) RunnerRunLog {
	if capPer <= 0 {
		capPer = 128
	}
	return &memoryRunLog{
		capPer:  capPer,
		entries: make(map[string][]RunLogEntry),
	}
}

func (l *memoryRunLog) Append(_ context.Context, entry RunLogEntry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	list := l.entries[entry.Name]
	list = append(list, entry)
	if over := len(list) - l.capPer; over > 0 {
		// drop the oldest `over` entries; keep tail
		list = append([]RunLogEntry(nil), list[over:]...)
	}
	l.entries[entry.Name] = list
	return nil
}

func (l *memoryRunLog) Finalize(_ context.Context, name string, seq int, finalized RunLogEntry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	list := l.entries[name]
	for i := len(list) - 1; i >= 0; i-- {
		if list[i].FireSeq != seq {
			continue
		}
		// Preserve Append-time fields that the caller may not
		// have re-populated on the finalized struct (PlannedAt,
		// StartedAt, Name, FireSeq). The Finalize contract
		// requires Status/ErrorMessage/Summary/CompletedAt/
		// Duration to be set by the caller.
		merged := list[i]
		merged.CompletedAt = finalized.CompletedAt
		merged.Status = finalized.Status
		merged.ErrorMessage = finalized.ErrorMessage
		merged.Summary = finalized.Summary
		merged.Duration = finalized.Duration
		list[i] = merged
		l.entries[name] = list
		return nil
	}
	// fire row vanished (overflowed the cap); append a synthetic
	// row so the finalize event is still observable.
	return l.Append(context.Background(), finalized)
}

func (l *memoryRunLog) ListByName(_ context.Context, name string, limit int) ([]RunLogEntry, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	src := l.entries[name]
	if len(src) == 0 {
		return nil, nil
	}
	if limit <= 0 || limit > len(src) {
		limit = len(src)
	}
	out := make([]RunLogEntry, limit)
	copy(out, src[len(src)-limit:])
	return out, nil
}

func (l *memoryRunLog) LatestSuccess(_ context.Context, name string) (RunLogEntry, bool, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	list := l.entries[name]
	for i := len(list) - 1; i >= 0; i-- {
		if list[i].Status == RunLogCompleted {
			return list[i], true, nil
		}
	}
	return RunLogEntry{}, false, nil
}
