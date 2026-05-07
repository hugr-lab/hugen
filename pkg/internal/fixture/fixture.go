package fixture

import (
	"context"
	"sort"
	"sync"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/store"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// TestSessionState is a minimal in-memory [extension.SessionState]
// for tests that drive extensions / tool providers without a real
// *session.Session. Value/SetValue are sync.Map-backed; Tools()
// returns whatever the test installs via [TestSessionState.SetTools]
// (nil by default). Parent linkage is set via
// [TestSessionState.WithParent].
type TestSessionState struct {
	id            string
	tools         *tool.ToolManager
	parentRef     *TestSessionState
	state         sync.Map
	workspaceDir  string
	workspaceRoot string

	emitMu  sync.Mutex
	emitted []protocol.Frame
}

// NewTestSessionState builds a TestSessionState bound to the given
// session id. Most tests use just this — they don't need parent
// or tools wiring.
func NewTestSessionState(sessionID string) *TestSessionState {
	return &TestSessionState{id: sessionID}
}

// WithParent wires a parent TestSessionState so Parent() returns
// it. Returns the receiver so callers can chain.
func (s *TestSessionState) WithParent(parent *TestSessionState) *TestSessionState {
	s.parentRef = parent
	return s
}

// SetTools installs a ToolManager that Tools() returns. Tests
// exercising dynamic-provider mounting via state.Tools().AddProvider
// pass a real manager here.
func (s *TestSessionState) SetTools(tm *tool.ToolManager) { s.tools = tm }

// SessionID implements [extension.SessionState].
func (s *TestSessionState) SessionID() string { return s.id }

// SetValue implements [extension.SessionState].
func (s *TestSessionState) SetValue(name string, value any) { s.state.Store(name, value) }

// Value implements [extension.SessionState].
func (s *TestSessionState) Value(name string) (any, bool) { return s.state.Load(name) }

// Parent implements [extension.SessionState]. Returns the wired
// parent or (nil, false) when no parent was attached via
// [TestSessionState.WithParent].
func (s *TestSessionState) Parent() (extension.SessionState, bool) {
	if s.parentRef == nil {
		return nil, false
	}
	return s.parentRef, true
}

// Tools implements [extension.SessionState]. Returns whatever was
// installed via [TestSessionState.SetTools]; nil by default.
func (s *TestSessionState) Tools() *tool.ToolManager { return s.tools }

// SetWorkspace installs absolute paths for WorkspaceDir /
// WorkspaceRoot. Tests exercising extensions that read workspace
// paths (e.g. mcp ext) wire this; default zero values surface as
// ok=false.
func (s *TestSessionState) SetWorkspace(sessionDir, root string) {
	s.workspaceDir = sessionDir
	s.workspaceRoot = root
}

// WorkspaceDir implements [extension.SessionState].
func (s *TestSessionState) WorkspaceDir() (string, bool) {
	return s.workspaceDir, s.workspaceDir != ""
}

// WorkspaceRoot implements [extension.SessionState].
func (s *TestSessionState) WorkspaceRoot() (string, bool) {
	return s.workspaceRoot, s.workspaceRoot != ""
}

// Emit implements [extension.SessionState]. Records the frame in
// memory so tests can assert what an extension emitted; the
// fixture is not wired to any real event store.
func (s *TestSessionState) Emit(_ context.Context, frame protocol.Frame) error {
	s.emitMu.Lock()
	defer s.emitMu.Unlock()
	s.emitted = append(s.emitted, frame)
	return nil
}

// Emitted returns a snapshot of every frame Emit has accepted, in
// emission order. Read-only — callers must not mutate the
// returned slice's frames.
func (s *TestSessionState) Emitted() []protocol.Frame {
	s.emitMu.Lock()
	defer s.emitMu.Unlock()
	out := make([]protocol.Frame, len(s.emitted))
	copy(out, s.emitted)
	return out
}

// Compile-time interface assertion.
var _ extension.SessionState = (*TestSessionState)(nil)

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
	Notes    []store.NoteRow
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

func (s *TestStore) AppendNote(_ context.Context, n store.NoteRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Notes = append(s.Notes, n)
	return nil
}

func (s *TestStore) ListNotes(_ context.Context, sessionID string, limit int) ([]store.NoteRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]store.NoteRow, 0, len(s.Notes))
	for _, n := range s.Notes {
		if sessionID != "" && n.SessionID != sessionID {
			continue
		}
		out = append(out, n)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
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
