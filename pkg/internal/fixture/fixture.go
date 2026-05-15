package fixture

import (
	"context"
	"io/fs"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hugr-lab/hugen/assets"
	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/prompts"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/store"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// defaultPromptsOnce caches a single bundled renderer for every
// TestSessionState constructed via NewTestSessionState. Tests that
// exercise prompt rendering through state.Prompts() reach the
// production templates without per-test scaffolding; tests that
// explicitly want a nil renderer can call SetPrompts(nil) after
// construction.
var (
	defaultPromptsOnce sync.Once
	defaultPromptsRdr  *prompts.Renderer
)

func defaultPrompts() *prompts.Renderer {
	defaultPromptsOnce.Do(func() {
		sub, err := fs.Sub(assets.PromptsFS, "prompts")
		if err != nil {
			return
		}
		defaultPromptsRdr = prompts.NewRenderer(sub, nil)
	})
	return defaultPromptsRdr
}

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
	prompts   *prompts.Renderer
	parentRef *TestSessionState
	depth     int
	state     sync.Map

	childMu  sync.Mutex
	children []*TestSessionState

	emitMu  sync.Mutex
	emitted []protocol.Frame

	inboxMu sync.Mutex
	inbox   []protocol.Frame
	closed  bool

	extensions []extension.Extension
}

// NewTestSessionState builds a TestSessionState bound to the given
// session id. Most tests use just this — they don't need parent
// or tools wiring. The default Prompts renderer is the bundled
// production templates (assets.PromptsFS with no override); tests
// that need a different renderer call SetPrompts on the result.
func NewTestSessionState(sessionID string) *TestSessionState {
	return &TestSessionState{
		id:      sessionID,
		prompts: defaultPrompts(),
	}
}

// WithParent wires a parent TestSessionState so Parent() returns
// it; also bumps Depth() one above the parent's so tier-aware
// extensions resolve correctly under test. Returns the receiver
// so callers can chain.
func (s *TestSessionState) WithParent(parent *TestSessionState) *TestSessionState {
	s.parentRef = parent
	if parent != nil {
		s.depth = parent.depth + 1
	}
	return s
}

// WithDepth overrides Depth() directly — useful when a test needs
// to simulate a tier mid-tree (e.g. a worker at depth 2) without
// constructing the full parent chain. Returns the receiver so
// callers can chain.
func (s *TestSessionState) WithDepth(depth int) *TestSessionState {
	s.depth = depth
	return s
}

// Depth implements [extension.SessionState]. Returns whatever was
// configured via [TestSessionState.WithParent] (parent.depth+1)
// or [TestSessionState.WithDepth]; 0 by default.
func (s *TestSessionState) Depth() int { return s.depth }

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

// IsClosed implements [extension.SessionState]. Reports the
// closed flag CloseInbox toggles.
func (s *TestSessionState) IsClosed() bool {
	s.inboxMu.Lock()
	defer s.inboxMu.Unlock()
	return s.closed
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

// SetPrompts installs a renderer that Prompts() returns. Tests
// exercising extension-side prose rendering pass a real renderer
// here; tests that only touch other surfaces leave it nil.
func (s *TestSessionState) SetPrompts(r *prompts.Renderer) { s.prompts = r }

// Prompts implements [extension.SessionState]. Returns whatever
// was installed via [TestSessionState.SetPrompts]; nil by default.
func (s *TestSessionState) Prompts() *prompts.Renderer { return s.prompts }

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

// Submit implements [extension.SessionState]. Synchronously
// appends frame to the in-memory inbox queue when the session is
// alive and ctx is live; returns an already-closed "settled"
// channel either way so the test sees the same shape as the
// production *Session.Submit (the goroutine + channel allocation
// is unnecessary in tests). Use [TestSessionState.CloseInbox] to
// simulate a terminated session — subsequent Submit drops the
// frame but still returns a closed channel.
func (s *TestSessionState) Submit(ctx context.Context, frame protocol.Frame) <-chan struct{} {
	settled := make(chan struct{})
	close(settled)
	if ctx.Err() != nil {
		return settled
	}
	s.inboxMu.Lock()
	defer s.inboxMu.Unlock()
	if s.closed {
		return settled
	}
	s.inbox = append(s.inbox, frame)
	return settled
}

// OutboxOnly implements [extension.SessionState]. Records the
// frame in the same `emitted` slice Emit uses — tests asserting
// "did the extension produce a status frame?" treat both paths
// uniformly. The fixture does not differentiate persisted vs
// outbox-only since it's not wired to any real store.
func (s *TestSessionState) OutboxOnly(_ context.Context, frame protocol.Frame) error {
	s.emitMu.Lock()
	defer s.emitMu.Unlock()
	s.emitted = append(s.emitted, frame)
	return nil
}

// Extensions implements [extension.SessionState]. Returns the
// extensions slice installed via [TestSessionState.SetExtensions];
// nil by default — tests that don't exercise aggregation skip
// the setter.
func (s *TestSessionState) Extensions() []extension.Extension {
	return s.extensions
}

// SetExtensions installs the slice [TestSessionState.Extensions]
// returns. Used by aggregator tests (liveview unit tests) that
// need to drive `state.Extensions()` for `StatusReporter`
// discovery.
func (s *TestSessionState) SetExtensions(exts []extension.Extension) {
	s.extensions = exts
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
	var kinds map[string]struct{}
	if len(opts.Kinds) > 0 {
		kinds = make(map[string]struct{}, len(opts.Kinds))
		for _, k := range opts.Kinds {
			kinds[k] = struct{}{}
		}
	}
	out := make([]store.EventRow, 0, len(src))
	for _, ev := range src {
		if opts.MinSeq > 0 && ev.Seq <= opts.MinSeq {
			continue
		}
		if kinds != nil {
			if _, ok := kinds[ev.EventType]; !ok {
				continue
			}
		}
		if !metadataContains(ev.Metadata, opts.MetadataContains) {
			continue
		}
		if !opts.From.IsZero() && ev.CreatedAt.Before(opts.From) {
			continue
		}
		if !opts.To.IsZero() && ev.CreatedAt.After(opts.To) {
			continue
		}
		out = append(out, ev)
	}
	// SemanticQuery is best-effort substring fallback in tests — the
	// fixture has no embedder, so we approximate ranking with a
	// case-insensitive content match. Production routes through Hugr's
	// `semantic:` argument when an embedder is attached.
	if opts.SemanticQuery != "" {
		needle := strings.ToLower(opts.SemanticQuery)
		matched := out[:0]
		for _, ev := range out {
			if strings.Contains(strings.ToLower(ev.Content), needle) {
				matched = append(matched, ev)
			}
		}
		out = matched
	}
	if opts.Limit > 0 && len(out) > opts.Limit {
		out = out[:opts.Limit]
	}
	return out, nil
}

// metadataContains mirrors Hugr's JSON `contains` (PostgreSQL `@>`)
// for the in-memory test store: returns true iff every key/value
// in want is present and equal in have. Empty want = match all.
func metadataContains(have, want map[string]any) bool {
	if len(want) == 0 {
		return true
	}
	if len(have) == 0 {
		return false
	}
	for k, w := range want {
		h, ok := have[k]
		if !ok || h != w {
			return false
		}
	}
	return true
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

func (s *TestStore) ListNotes(_ context.Context, sessionID string, opts store.ListNotesOpts) ([]store.NoteRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	cutoff := time.Time{}
	if opts.Window > 0 {
		cutoff = time.Now().UTC().Add(-opts.Window)
	}
	// Snapshot newest-first; the production store's GraphQL query
	// uses created_at DESC. The fixture iterates in append order so
	// reverse on the way out.
	out := make([]store.NoteRow, 0, len(s.Notes))
	for i := len(s.Notes) - 1; i >= 0; i-- {
		n := s.Notes[i]
		if sessionID != "" && n.SessionID != sessionID {
			continue
		}
		if opts.Category != "" && n.Category != opts.Category {
			continue
		}
		if !cutoff.IsZero() && n.CreatedAt.Before(cutoff) {
			continue
		}
		out = append(out, n)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// SearchNotes — fixture implementation has no embedder, so it
// degrades to ErrNoEmbedder. Tests that want a real semantic
// path use the live local store with an embedder data source.
func (s *TestStore) SearchNotes(_ context.Context, _, _ string, _ store.ListNotesOpts) ([]store.NoteRow, error) {
	return nil, store.ErrNoEmbedder
}

// SessionStats reports the in-memory event count for sessionID.
// Phase 5.1c S2 — the fixture mirrors the production accessor.
func (s *TestStore) SessionStats(_ context.Context, sessionID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Events[sessionID]), nil
}

// CountNotesByCategory mirrors the production store: groups by
// category, honouring opts.Window and opts.Category. opts.Limit
// is ignored — bucket cardinality is bounded by distinct
// category count.
func (s *TestStore) CountNotesByCategory(_ context.Context, sessionID string, opts store.ListNotesOpts) (map[string]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Time{}
	if opts.Window > 0 {
		cutoff = time.Now().UTC().Add(-opts.Window)
	}
	out := map[string]int{}
	for _, n := range s.Notes {
		if sessionID != "" && n.SessionID != sessionID {
			continue
		}
		if opts.Category != "" && n.Category != opts.Category {
			continue
		}
		if !cutoff.IsZero() && n.CreatedAt.Before(cutoff) {
			continue
		}
		out[n.Category]++
	}
	if len(out) == 0 {
		return nil, nil
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

func (s *TestStore) ListResumableRoots(_ context.Context, agentID string) ([]store.ResumableRoot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]store.ResumableRoot, 0, len(s.Sessions))
	for _, row := range s.Sessions {
		if row.AgentID != agentID {
			continue
		}
		if row.SessionType != "" && row.SessionType != "root" {
			continue
		}
		if row.Status != store.StatusActive {
			continue
		}
		// Pick the latest KindSessionStatus event for this row,
		// mirroring the production query's nested filter (limit=1,
		// order_by created_at DESC). Events arrive in append order
		// — walk backwards.
		var lifecycle []store.EventRow
		evs := s.Events[row.ID]
		for i := len(evs) - 1; i >= 0; i-- {
			if protocol.Kind(evs[i].EventType) == protocol.KindSessionStatus {
				lifecycle = []store.EventRow{evs[i]}
				break
			}
		}
		out = append(out, store.ResumableRoot{SessionRow: row, Lifecycle: lifecycle})
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
