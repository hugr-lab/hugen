package session

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/identity"
	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/internal/fixture"
	"github.com/hugr-lab/hugen/pkg/tool"
)

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
	agent, err := NewAgent("a1", "hugen", &fakeIdentity{id: "a1"}, "", nil)
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
	tm := tool.NewToolManager(permsAllow{}, nil, nil)
	sess := NewSession("s1", agent, store, router, cmds, codec, tm, nil)
	sess.materialised.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = sess.Run(ctx) }()
	return sess, cancel
}

func TestSession_HappyPathTurn_FrameSequence(t *testing.T) {
	testStore := fixture.NewTestStore()
	_ = testStore.OpenSession(context.Background(), SessionRow{ID: "s1", AgentID: "a1", Status: StatusActive})

	mdl := &scriptedModel{
		chunks: []model.Chunk{
			{Reasoning: ptr("thinking...")},
			{Content: ptr("Hello")},
			{Content: ptr(", world!"), Final: true},
		},
	}
	sess, cancel := newTestSession(t, testStore, mdl)
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
		string(protocol.KindSessionStatus), // active marker emitted by startTurn
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
	rec := testStore.RecordedKinds()
	// Streaming chunks (Reasoning + AgentMessage with Consolidated=false)
	// are outbox-only by design: live UX renders them, but only the
	// per-iteration consolidated AgentMessage row lands in the store.
	// So persistence rows are a SUBSET of outbox frames; we assert the
	// non-streaming frames are all present.
	if got, want := countKind(rec, string(protocol.KindAgentMessage)), 1; got != want {
		t.Errorf("persisted agent_message rows = %d, want %d (consolidated only)", got, want)
	}
	if got := countKind(rec, string(protocol.KindReasoning)); got != 0 {
		t.Errorf("persisted reasoning rows = %d, want 0 (chunks are outbox-only)", got)
	}
	if !contains(rec, string(protocol.KindUserMessage)) {
		t.Errorf("persisted rows missing user_message: %v", rec)
	}
}

func countKind(kinds []string, want string) int {
	n := 0
	for _, k := range kinds {
		if k == want {
			n++
		}
	}
	return n
}

func TestSession_NoModelInvocationForSlashCommand(t *testing.T) {
	testStore := fixture.NewTestStore()
	_ = testStore.OpenSession(context.Background(), SessionRow{ID: "s1", AgentID: "a1", Status: StatusActive})
	mdl := &scriptedModel{}
	sess, cancel := newTestSession(t, testStore, mdl)
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
	testStore := fixture.NewTestStore()
	_ = testStore.OpenSession(context.Background(), SessionRow{ID: "s1", AgentID: "a1", Status: StatusActive})
	mdl := &scriptedModel{}
	sess, cancel := newTestSession(t, testStore, mdl)
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
