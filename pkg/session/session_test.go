package session

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/identity"
	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// fakeStore is a minimal in-memory RuntimeStore. It supports multiple
// sessions so phase-4 spawn / restart tests can build small parent /
// child trees against it.
//
// Single-session tests still work: the legacy `session` field aliases
// the first OpenSession call so any test that opens one session and
// reads via LoadSession/UpdateSessionStatus continues to behave as
// before (for backward compatibility with phase 1-3.5 test cases).
type fakeStore struct {
	mu       sync.Mutex
	events   map[string][]EventRow // by sessionID
	notes    []NoteRow
	sessions map[string]SessionRow // by sessionID
	seq      map[string]int        // per-session seq cursor
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		events:   make(map[string][]EventRow),
		sessions: make(map[string]SessionRow),
		seq:      make(map[string]int),
	}
}

func (s *fakeStore) OpenSession(_ context.Context, row SessionRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[row.ID] = row
	return nil
}

func (s *fakeStore) LoadSession(_ context.Context, id string) (SessionRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.sessions[id]
	if !ok {
		return SessionRow{}, ErrSessionNotFound
	}
	return row, nil
}

func (s *fakeStore) UpdateSessionStatus(_ context.Context, id, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.sessions[id]
	if !ok {
		return ErrSessionNotFound
	}
	row.Status = status
	s.sessions[id] = row
	return nil
}

func (s *fakeStore) AppendEvent(_ context.Context, ev EventRow, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq[ev.SessionID]++
	ev.Seq = s.seq[ev.SessionID]
	s.events[ev.SessionID] = append(s.events[ev.SessionID], ev)
	return nil
}

func (s *fakeStore) ListEvents(_ context.Context, sessionID string, opts ListEventsOpts) ([]EventRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.events[sessionID]
	out := make([]EventRow, 0, len(src))
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

func (s *fakeStore) NextSeq(_ context.Context, sessionID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq[sessionID] + 1, nil
}

func (s *fakeStore) AppendNote(_ context.Context, n NoteRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notes = append(s.notes, n)
	return nil
}

func (s *fakeStore) ListNotes(_ context.Context, _ string, _ int) ([]NoteRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]NoteRow(nil), s.notes...), nil
}

func (s *fakeStore) ListSessions(_ context.Context, _, _ string) ([]SessionRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sessions) == 0 {
		return nil, nil
	}
	out := make([]SessionRow, 0, len(s.sessions))
	for _, row := range s.sessions {
		out = append(out, row)
	}
	return out, nil
}

func (s *fakeStore) ListChildren(_ context.Context, parentID string) ([]SessionRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SessionRow, 0)
	for _, row := range s.sessions {
		if row.ParentSessionID == parentID {
			out = append(out, row)
		}
	}
	return out, nil
}

func (s *fakeStore) recordedKinds() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Existing single-session callers expect a flat list of event types
	// in arrival order. With the map backing, sort by sessionID + seq
	// to keep the output deterministic across runs.
	type rec struct {
		sid string
		ev  EventRow
	}
	all := make([]rec, 0)
	for sid, evs := range s.events {
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

// scriptedModel emits a fixed sequence of chunks then ends.
type scriptedModel struct {
	chunks []model.Chunk
	mu     sync.Mutex
	calls  int
}

func (m *scriptedModel) Spec() model.ModelSpec {
	return model.ModelSpec{Provider: "fake", Name: "test"}
}

func (m *scriptedModel) Generate(_ context.Context, _ model.Request) (model.Stream, error) {
	m.mu.Lock()
	m.calls++
	chunks := append([]model.Chunk(nil), m.chunks...)
	m.mu.Unlock()
	ch := make(chan model.Chunk, len(chunks))
	for _, c := range chunks {
		ch <- c
	}
	close(ch)
	return &scriptedStream{ch: ch}, nil
}

func (m *scriptedModel) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

type scriptedStream struct {
	ch   chan model.Chunk
	done bool
}

func (s *scriptedStream) Next(_ context.Context) (model.Chunk, bool, error) {
	c, ok := <-s.ch
	if !ok {
		return model.Chunk{}, false, nil
	}
	return c, true, nil
}

func (s *scriptedStream) Close() error { s.done = true; return nil }

// fakeIdentity satisfies pkg/identity.Source for tests.
type fakeIdentity struct{ id string }

func (f *fakeIdentity) Agent(_ context.Context) (identity.Agent, error) {
	return identity.Agent{ID: f.id, Name: "hugen", Type: "test"}, nil
}
func (f *fakeIdentity) WhoAmI(_ context.Context) (identity.WhoAmI, error) {
	return identity.WhoAmI{UserID: f.id, UserName: "hugen", Role: "agent"}, nil
}
func (f *fakeIdentity) Permission(_ context.Context, _, _ string) (identity.Permission, error) {
	return identity.Permission{Enabled: true}, nil
}

func newRouterWithModel(t *testing.T, m model.Model) *model.ModelRouter {
	t.Helper()
	defaults := map[model.Intent]model.ModelSpec{
		model.IntentDefault: m.Spec(),
		model.IntentCheap:   m.Spec(),
	}
	models := map[model.ModelSpec]model.Model{m.Spec(): m}
	r, err := model.NewModelRouter(defaults, models)
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	return r
}

func ptr[T any](v T) *T { return &v }

func newTestSession(t *testing.T, store RuntimeStore, mdl model.Model) (*Session, context.CancelFunc) {
	t.Helper()
	router := newRouterWithModel(t, mdl)
	agent, err := NewAgent("a1", "hugen", &fakeIdentity{id: "a1"}, "")
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	cmds := NewCommandRegistry()
	_ = cmds.Register("ping", CommandSpec{
		Description: "test",
		Handler: func(_ context.Context, env CommandEnv, _ []string) ([]protocol.Frame, error) {
			return []protocol.Frame{
				protocol.NewAgentMessage(env.Session.ID(), env.AgentAuthor, "pong", 0, true),
			}, nil
		},
	})
	codec := protocol.NewCodec()
	sess := NewSession("s1", agent, store, router, cmds, codec, nil)
	sess.materialised.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = sess.Run(ctx) }()
	return sess, cancel
}

func TestSession_HappyPathTurn_FrameSequence(t *testing.T) {
	store := newFakeStore()
	_ = store.OpenSession(context.Background(), SessionRow{ID: "s1", AgentID: "a1", Status: StatusActive})

	mdl := &scriptedModel{
		chunks: []model.Chunk{
			{Reasoning: ptr("thinking...")},
			{Content: ptr("Hello")},
			{Content: ptr(", world!"), Final: true},
		},
	}
	sess, cancel := newTestSession(t, store, mdl)
	defer cancel()

	user := protocol.ParticipantInfo{ID: "u1", Kind: protocol.ParticipantUser, Name: "alice"}
	sess.Inbox() <- protocol.NewUserMessage("s1", user, "hi")

	var seen []string
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case f, ok := <-sess.Outbox():
			if !ok {
				goto done
			}
			seen = append(seen, string(f.Kind()))
			if am, ok := f.(*protocol.AgentMessage); ok && am.Payload.Final {
				goto done
			}
		case <-deadline.C:
			t.Fatalf("timeout; seen=%v", seen)
		}
	}
done:

	wantOrder := []string{
		string(protocol.KindUserMessage),
		string(protocol.KindReasoning),
		string(protocol.KindAgentMessage),
		string(protocol.KindAgentMessage),
	}
	if len(seen) < len(wantOrder) {
		t.Fatalf("not enough frames: got %v, want at least %v", seen, wantOrder)
	}
	for i, want := range wantOrder {
		if seen[i] != want {
			t.Errorf("frame[%d] = %q, want %q (seen=%v)", i, seen[i], want, seen)
		}
	}
	if mdl.callCount() != 1 {
		t.Errorf("model.Generate calls = %d, want 1", mdl.callCount())
	}
	rec := store.recordedKinds()
	if len(rec) < len(seen) {
		t.Errorf("persistence rows %d < emitted frames %d", len(rec), len(seen))
	}
}

func TestSession_NoModelInvocationForSlashCommand(t *testing.T) {
	store := newFakeStore()
	_ = store.OpenSession(context.Background(), SessionRow{ID: "s1", AgentID: "a1", Status: StatusActive})
	mdl := &scriptedModel{}
	sess, cancel := newTestSession(t, store, mdl)
	defer cancel()

	user := protocol.ParticipantInfo{ID: "u1", Kind: protocol.ParticipantUser}
	sess.Inbox() <- protocol.NewSlashCommand("s1", user, "ping", nil, "/ping")

	deadline := time.NewTimer(1 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case f := <-sess.Outbox():
			if am, ok := f.(*protocol.AgentMessage); ok && am.Payload.Final {
				if mdl.callCount() != 0 {
					t.Errorf("expected zero model calls, got %d", mdl.callCount())
				}
				return
			}
		case <-deadline.C:
			t.Fatal("timeout waiting for /ping reply")
		}
	}
}

func TestSession_UnknownSlashCommandEmitsError(t *testing.T) {
	store := newFakeStore()
	_ = store.OpenSession(context.Background(), SessionRow{ID: "s1", AgentID: "a1", Status: StatusActive})
	mdl := &scriptedModel{}
	sess, cancel := newTestSession(t, store, mdl)
	defer cancel()

	user := protocol.ParticipantInfo{ID: "u1", Kind: protocol.ParticipantUser}
	sess.Inbox() <- protocol.NewSlashCommand("s1", user, "wat", nil, "/wat")

	deadline := time.NewTimer(1 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case f := <-sess.Outbox():
			if e, ok := f.(*protocol.Error); ok {
				if e.Payload.Code != "unknown_command" {
					t.Errorf("error code = %q, want unknown_command", e.Payload.Code)
				}
				return
			}
		case <-deadline.C:
			t.Fatal("timeout waiting for error frame")
		}
	}
}
