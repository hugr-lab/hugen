package fixture

import (
	"context"
	"sort"
	"sync"

	"github.com/hugr-lab/hugen/pkg/session/store"
	"github.com/hugr-lab/hugen/pkg/session/tools/notepad"
)

// TestStore is a minimal in-memory RuntimeStore. It supports multiple
// sessions so phase-4 spawn / restart tests can build small parent /
// child trees against it.
//
// Single-session tests still work: the legacy `session` field aliases
// the first OpenSession call so any test that opens one session and
// reads via LoadSession/UpdateSessionStatus continues to behave as
// before (for backward compatibility with phase 1-3.5 test cases).
type TestStore struct {
	mu       sync.Mutex
	Events   map[string][]store.EventRow // by sessionID
	Notes    []notepad.NoteRow
	Sessions map[string]store.SessionRow // by sessionID
	Seq      map[string]int              // per-session seq cursor
}

func NewTestStore() *TestStore {
	return &TestStore{
		Events:   make(map[string][]store.EventRow),
		Sessions: make(map[string]store.SessionRow),
		Seq:      make(map[string]int),
	}
}

func (s *TestStore) OpenSession(_ context.Context, row store.SessionRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Sessions[row.ID] = row
	return nil
}

func (s *TestStore) LoadSession(_ context.Context, id string) (store.SessionRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.Sessions[id]
	if !ok {
		return store.SessionRow{}, store.ErrSessionNotFound
	}
	return row, nil
}

func (s *TestStore) UpdateSessionStatus(_ context.Context, id, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.Sessions[id]
	if !ok {
		return store.ErrSessionNotFound
	}
	row.Status = status
	s.Sessions[id] = row
	return nil
}

func (s *TestStore) AppendEvent(_ context.Context, ev store.EventRow, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Seq[ev.SessionID]++
	ev.Seq = s.Seq[ev.SessionID]
	s.Events[ev.SessionID] = append(s.Events[ev.SessionID], ev)
	return nil
}

func (s *TestStore) ListEvents(_ context.Context, sessionID string, opts store.ListEventsOpts) ([]store.EventRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.Events[sessionID]
	out := make([]store.EventRow, 0, len(src))
	for _, ev := range src {
		if opts.MinSeq > 0 && ev.Seq <= opts.MinSeq {
			continue
		}
		out = append(out, ev)
	}
	if opts.Limit > 0 && len(out) > opts.Limit {
		out = out[:opts.Limit]
	}
	return out, nil
}

func (s *TestStore) NextSeq(_ context.Context, sessionID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Seq[sessionID] + 1, nil
}

func (s *TestStore) AppendNote(_ context.Context, n notepad.NoteRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Notes = append(s.Notes, n)
	return nil
}

func (s *TestStore) ListNotes(_ context.Context, _ string, _ int) ([]notepad.NoteRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]notepad.NoteRow(nil), s.Notes...), nil
}

func (s *TestStore) ListSessions(_ context.Context, _, _ string) ([]store.SessionRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.Sessions) == 0 {
		return nil, nil
	}
	out := make([]store.SessionRow, 0, len(s.Sessions))
	for _, row := range s.Sessions {
		out = append(out, row)
	}
	return out, nil
}

func (s *TestStore) ListChildren(_ context.Context, parentID string) ([]store.SessionRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]store.SessionRow, 0)
	for _, row := range s.Sessions {
		if row.ParentSessionID == parentID {
			out = append(out, row)
		}
	}
	return out, nil
}

func (s *TestStore) RecordedKinds() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Existing single-session callers expect a flat list of event types
	// in arrival order. With the map backing, sort by sessionID + seq
	// to keep the output deterministic across runs.
	type rec struct {
		sid string
		ev  store.EventRow
	}
	all := make([]rec, 0)
	for sid, evs := range s.Events {
		for _, e := range evs {
			all = append(all, rec{sid: sid, ev: e})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].sid != all[j].sid {
			return all[i].sid < all[j].sid
		}
		return all[i].ev.Seq < all[j].ev.Seq
	})
	out := make([]string, 0, len(all))
	for _, r := range all {
		out = append(out, r.ev.EventType)
	}
	return out
}
