package scheduler

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
	schedstore "github.com/hugr-lab/hugen/pkg/scheduler/store"
	"github.com/hugr-lab/hugen/pkg/session"
)

// fakeStore is an in-memory TaskStore for scheduler-package unit
// tests — keeps the suite fast (no DuckDB spin-up) and lets us
// inject targeted failure modes via the hook fields.
type fakeStore struct {
	mu          sync.Mutex
	tasks       map[string]schedstore.TaskRow
	log         []schedstore.TaskLogEntry
	openFailErr error
	logFailErr  error
}

func newFakeStore() *fakeStore {
	return &fakeStore{tasks: map[string]schedstore.TaskRow{}}
}

func (s *fakeStore) OpenTask(_ context.Context, row schedstore.TaskRow, initial time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.openFailErr != nil {
		return s.openFailErr
	}
	if _, exists := s.tasks[row.ID]; exists {
		return schedstore.ErrTaskDuplicate
	}
	if row.Status == "" {
		row.Status = schedstore.StatusActive
	}
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now().UTC()
	}
	row.UpdatedAt = row.CreatedAt
	s.tasks[row.ID] = row
	s.log = append(s.log, schedstore.TaskLogEntry{
		TaskID:    row.ID,
		AgentID:   row.AgentID,
		FireSeq:   1,
		EventType: schedstore.LogEventPlanned,
		PlannedAt: initial,
		CreatedAt: row.CreatedAt,
	})
	return nil
}

func (s *fakeStore) GetTask(_ context.Context, id string) (schedstore.TaskRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.tasks[id]
	if !ok {
		return schedstore.TaskRow{}, schedstore.ErrTaskNotFound
	}
	return row, nil
}

func (s *fakeStore) ListTasksBySession(_ context.Context, sessionID string, opts schedstore.ListTasksOpts) ([]schedstore.TaskRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []schedstore.TaskRow{}
	for _, row := range s.tasks {
		if row.OwnerSessionID != sessionID {
			continue
		}
		if opts.Status != "" && row.Status != opts.Status {
			continue
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if opts.Limit > 0 && len(out) > opts.Limit {
		out = out[:opts.Limit]
	}
	return out, nil
}

func (s *fakeStore) CountTasksBySession(_ context.Context, sessionID string) (map[string]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int{}
	for _, row := range s.tasks {
		if row.OwnerSessionID == sessionID {
			out[row.Status]++
		}
	}
	return out, nil
}

func (s *fakeStore) ListDue(_ context.Context, _ string, _ time.Time, _ int) ([]schedstore.TaskRow, error) {
	return nil, errors.New("fakeStore: ListDue not implemented for these tests")
}

func (s *fakeStore) DeleteTask(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tasks[id]; !ok {
		return schedstore.ErrTaskNotFound
	}
	delete(s.tasks, id)
	return nil
}

func (s *fakeStore) PauseTask(_ context.Context, id, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.tasks[id]
	if !ok {
		return schedstore.ErrTaskNotFound
	}
	row.Status = schedstore.StatusPaused
	row.PauseReason = reason
	row.UpdatedAt = time.Now().UTC()
	s.tasks[id] = row
	return nil
}

func (s *fakeStore) ResumeTask(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.tasks[id]
	if !ok {
		return schedstore.ErrTaskNotFound
	}
	row.Status = schedstore.StatusActive
	row.PauseReason = ""
	row.UpdatedAt = time.Now().UTC()
	s.tasks[id] = row
	return nil
}

func (s *fakeStore) CancelTask(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.tasks[id]
	if !ok {
		return schedstore.ErrTaskNotFound
	}
	row.Status = schedstore.StatusCancelled
	row.UpdatedAt = time.Now().UTC()
	s.tasks[id] = row
	return nil
}

func (s *fakeStore) UpdateTaskSpec(_ context.Context, id string, spec schedstore.TaskSpec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.tasks[id]
	if !ok {
		return schedstore.ErrTaskNotFound
	}
	row.Spec = spec
	row.UpdatedAt = time.Now().UTC()
	s.tasks[id] = row
	return nil
}

func (s *fakeStore) AppendLog(_ context.Context, entry schedstore.TaskLogEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.logFailErr != nil {
		return s.logFailErr
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now().UTC()
	}
	s.log = append(s.log, entry)
	return nil
}

func (s *fakeStore) ListLogByTask(_ context.Context, taskID string, opts schedstore.ListLogOpts) ([]schedstore.TaskLogEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []schedstore.TaskLogEntry{}
	for _, entry := range s.log {
		if entry.TaskID != taskID {
			continue
		}
		if opts.SinceFireSeq > 0 && entry.FireSeq < opts.SinceFireSeq {
			continue
		}
		if len(opts.EventTypes) > 0 {
			match := false
			for _, et := range opts.EventTypes {
				if entry.EventType == et {
					match = true
					break
				}
			}
			if !match {
				continue
			}
		}
		out = append(out, entry)
	}
	// (fire_seq DESC, created_at DESC) per spec
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].FireSeq != out[j].FireSeq {
			return out[i].FireSeq > out[j].FireSeq
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	if opts.Limit > 0 && len(out) > opts.Limit {
		out = out[:opts.Limit]
	}
	return out, nil
}

func (s *fakeStore) LatestPlannedFire(_ context.Context, taskID string) (*schedstore.TaskLogEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best *schedstore.TaskLogEntry
	for i := range s.log {
		entry := s.log[i]
		if entry.TaskID != taskID || entry.EventType != schedstore.LogEventPlanned {
			continue
		}
		if best == nil || entry.FireSeq > best.FireSeq ||
			(entry.FireSeq == best.FireSeq && entry.CreatedAt.After(best.CreatedAt)) {
			cp := entry
			best = &cp
		}
	}
	return best, nil
}

func (s *fakeStore) LatestSuccessfulFire(_ context.Context, taskID string) (*schedstore.TaskLogEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best *schedstore.TaskLogEntry
	for i := range s.log {
		entry := s.log[i]
		if entry.TaskID != taskID || entry.EventType != schedstore.LogEventCompleted {
			continue
		}
		if best == nil || entry.FireSeq > best.FireSeq {
			cp := entry
			best = &cp
		}
	}
	return best, nil
}

func (s *fakeStore) ListInFlightFires(_ context.Context, _ string, _ time.Time) ([]schedstore.TaskLogEntry, error) {
	return nil, nil
}

func (s *fakeStore) snapshotLog() []schedstore.TaskLogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]schedstore.TaskLogEntry, len(s.log))
	copy(cp, s.log)
	return cp
}

// fakeHost records every Deliver / Get call. Spawn-fire tests
// don't drive Open anymore — fire fn calls owner.Spawn directly
// on the *Session returned by Get. For unit tests we still return
// (nil, alive) from Get because actually constructing a *Session
// requires booting the whole session machinery; spawn-fire tests
// that need a real session use the [pkg/internal/fixture] harness
// (separate integration-style coverage).
type fakeHost struct {
	mu         sync.Mutex
	alive      map[string]struct{}
	delivered  []deliveredFrame
	deliverErr error // when set, Deliver fails with it (delivery-failure tests)
}

type deliveredFrame struct {
	To    string
	Frame protocol.Frame
}

func newFakeHost() *fakeHost { return &fakeHost{alive: map[string]struct{}{}} }

func (h *fakeHost) markAlive(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.alive[id] = struct{}{}
}

func (h *fakeHost) Deliver(_ context.Context, to string, f protocol.Frame) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.deliverErr != nil {
		return h.deliverErr
	}
	h.delivered = append(h.delivered, deliveredFrame{To: to, Frame: f})
	return nil
}

func (h *fakeHost) Get(id string) (*session.Session, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.alive[id]
	return nil, ok
}

func (h *fakeHost) deliveries() []deliveredFrame {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]deliveredFrame, len(h.delivered))
	copy(out, h.delivered)
	return out
}
