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
// [TestSessionState.WithParent]; child linkage via
// [TestSessionState.AppendChild]. Submit records the inbound frame
// in a queue per session for assertions; closed sessions reject
// Submit.
type TestSessionState struct {
	id        string
	tools     *tool.ToolManager
	parentRef *TestSessionState
	state     sync.Map

	childMu  sync.Mutex
	children []*TestSessionState

	emitMu  sync.Mutex
	emitted []protocol.Frame

	inboxMu sync.Mutex
	inbox   []protocol.Frame
	closed  bool
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

// AppendChild registers child as a direct child so Children()
// returns it. Idempotent on duplicate appends. Returns the
// receiver so callers can chain.
func (s *TestSessionState) AppendChild(child *TestSessionState) *TestSessionState {
	if child == nil {
		return s
	}
	s.childMu.Lock()
	defer s.childMu.Unlock()
	for _, c := range s.children {
		if c == child {
			return s
		}
	}
	s.children = append(s.children, child)
	return s
}

// CloseInbox marks the session inbox closed; subsequent Submit
// calls return false. Tests use this to simulate a terminated
// session.
func (s *TestSessionState) CloseInbox() {
	s.inboxMu.Lock()
	defer s.inboxMu.Unlock()
	s.closed = true
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

// Children implements [extension.SessionState]. Returns a
// snapshot of every child registered via AppendChild, or nil.
func (s *TestSessionState) Children() []extension.SessionState {
	s.childMu.Lock()
	defer s.childMu.Unlock()
	if len(s.children) == 0 {
		return nil
	}
	out := make([]extension.SessionState, 0, len(s.children))
	for _, c := range s.children {
		out = append(out, c)
	}
	return out
}

// Tools implements [extension.SessionState]. Returns whatever was
// installed via [TestSessionState.SetTools]; nil by default.
func (s *TestSessionState) Tools() *tool.ToolManager { return s.tools }


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

// Submit implements [extension.SessionState]. Appends frame to
// the in-memory inbox queue or returns false if the inbox was
// closed via CloseInbox. ctx is honoured: a cancelled ctx
// returns false without recording the frame.
func (s *TestSessionState) Submit(ctx context.Context, frame protocol.Frame) bool {
	if err := ctx.Err(); err != nil {
		return false
	}
	s.inboxMu.Lock()
	defer s.inboxMu.Unlock()
	if s.closed {
		return false
	}
	s.inbox = append(s.inbox, frame)
	return true
}

// Inbox returns a snapshot of every frame Submit accepted, in
// submission order. Read-only — callers must not mutate the
// returned slice's frames.
func (s *TestSessionState) Inbox() []protocol.Frame {
	s.inboxMu.Lock()
	defer s.inboxMu.Unlock()
	out := make([]protocol.Frame, len(s.inbox))
	copy(out, s.inbox)
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

// LatestEventOfKinds returns the newest event in the session
// whose EventType matches one of kinds. Backs RestoreActive's
// narrow classifier query.
func (s *TestStore) LatestEventOfKinds(_ context.Context, sessionID string, kinds []string) (store.EventRow, bool, error) {
	if len(kinds) == 0 {
		return store.EventRow{}, false, nil
	}
	want := make(map[string]struct{}, len(kinds))
	for _, k := range kinds {
		want[k] = struct{}{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.Events[sessionID]
	for i := len(src) - 1; i >= 0; i-- {
		if _, ok := want[src[i].EventType]; ok {
			return src[i], true, nil
		}
	}
	return store.EventRow{}, false, nil
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
