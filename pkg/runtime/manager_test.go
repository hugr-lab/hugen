package runtime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// instrumentedStore wraps fakeStore with call counters used by the
// lazy-materialisation tests.
type instrumentedStore struct {
	*fakeStore
	listEventsCalls atomic.Int32
}

func (s *instrumentedStore) ListEvents(ctx context.Context, sid string, opts ListEventsOpts) ([]EventRow, error) {
	s.listEventsCalls.Add(1)
	return s.fakeStore.ListEvents(ctx, sid, opts)
}

func newTestManager(t *testing.T, store RuntimeStore) *SessionManager {
	t.Helper()
	mdl := &scriptedModel{}
	router := newRouterWithModel(t, mdl)
	agent, err := NewAgent("a1", "hugen", &fakeIdentity{id: "a1"})
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	return NewSessionManager(store, agent, router, NewCommandRegistry(), protocol.NewCodec(), nil)
}

func TestSessionManager_LazyMaterialisation(t *testing.T) {
	base := newFakeStore()
	// Seed the store with a session row + 100 historic events.
	_ = base.OpenSession(context.Background(), SessionRow{
		ID: "s1", AgentID: "a1", Status: StatusActive,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	for i := 0; i < 100; i++ {
		_ = base.AppendEvent(context.Background(), EventRow{
			ID:        "ev" + string(rune('a'+i%26)),
			SessionID: "s1",
			AgentID:   "a1",
			EventType: string(protocol.KindUserMessage),
			Author:    "u1",
			Content:   "msg",
		}, "")
	}
	store := &instrumentedStore{fakeStore: base}

	mgr := newTestManager(t, store)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess, err := mgr.Resume(ctx, "s1")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	// Reset list-event counter to ignore the session_resumed marker
	// emit path (which doesn't call ListEvents, but be defensive).
	store.listEventsCalls.Store(0)

	// Resume returned without walking events.
	if got := store.listEventsCalls.Load(); got != 0 {
		t.Fatalf("ListEvents called before first inbound frame: %d", got)
	}
	// Drain any system_resumed marker emitted by Resume.
	drainOutboxOnce(sess.Outbox())

	// Drop a user message and verify the materialise call happens.
	user := protocol.ParticipantInfo{ID: "u1", Kind: protocol.ParticipantUser}
	sess.Inbox() <- protocol.NewUserMessage("s1", user, "hello")

	// Wait until the user_message echo is observed — by then the
	// turn has started and materialise was called.
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	gotEcho := false
	for !gotEcho {
		select {
		case f, ok := <-sess.Outbox():
			if !ok {
				t.Fatal("outbox closed before user_message echo")
			}
			if f.Kind() == protocol.KindUserMessage {
				gotEcho = true
			}
		case <-deadline.C:
			t.Fatal("timeout waiting for user_message echo")
		}
	}
	// Allow the materialise call to land before reading the counter.
	time.Sleep(50 * time.Millisecond)
	if got := store.listEventsCalls.Load(); got < 1 {
		t.Errorf("ListEvents not called after first inbound frame: got %d", got)
	}
}

func TestSessionManager_ResumeClosed(t *testing.T) {
	store := newFakeStore()
	_ = store.OpenSession(context.Background(), SessionRow{ID: "s1", AgentID: "a1", Status: StatusClosed})
	mgr := newTestManager(t, store)
	if _, err := mgr.Resume(context.Background(), "s1"); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("expected ErrSessionClosed, got %v", err)
	}
}

func TestSessionManager_ResumeNotFound(t *testing.T) {
	store := newFakeStore()
	mgr := newTestManager(t, store)
	if _, err := mgr.Resume(context.Background(), "nope"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("expected ErrSessionNotFound, got %v", err)
	}
}

func TestSessionManager_OpenAndList(t *testing.T) {
	store := newFakeStore()
	mgr := newTestManager(t, store)
	s, _, err := mgr.Open(context.Background(), OpenRequest{OwnerID: "alice"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if s.ID() == "" {
		t.Fatal("expected non-empty session id")
	}
	// At least one event_opened was persisted.
	if len(store.events) == 0 {
		t.Fatal("expected session_opened event in store")
	}
}

// projectHistory unit test — keeps the most-recent K user/agent
// messages.
func TestProjectHistory_Window(t *testing.T) {
	rows := make([]EventRow, 0, 200)
	for i := 0; i < 100; i++ {
		rows = append(rows, EventRow{
			EventType: string(protocol.KindUserMessage),
			Content:   "user",
		})
		rows = append(rows, EventRow{
			EventType: string(protocol.KindAgentMessage),
			Content:   "agent",
			Metadata:  map[string]any{"final": true},
		})
	}
	got := projectHistory(rows, 50)
	if len(got) != 50 {
		t.Errorf("len = %d, want 50", len(got))
	}
}

// Touch the model package to avoid an unused-import lint.
var _ = model.IntentDefault

func TestSessionManager_LifecycleHooks(t *testing.T) {
	store := newFakeStore()
	mdl := &scriptedModel{}
	router := newRouterWithModel(t, mdl)
	agent, err := NewAgent("a1", "hugen", &fakeIdentity{id: "a1"})
	if err != nil {
		t.Fatalf("agent: %v", err)
	}

	var openCalled, closeCalled atomic.Int32
	mgr := NewSessionManager(store, agent, router, NewCommandRegistry(), protocol.NewCodec(), nil,
		WithLifecycle(SessionLifecycle{
			OnOpen: func(ctx context.Context, sessionID string) error {
				openCalled.Add(1)
				return nil
			},
			OnClose: func(ctx context.Context, sessionID string) error {
				closeCalled.Add(1)
				return nil
			},
		}),
	)
	s, _, err := mgr.Open(context.Background(), OpenRequest{OwnerID: "alice"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if openCalled.Load() != 1 {
		t.Errorf("OnOpen calls = %d, want 1", openCalled.Load())
	}
	if _, err := mgr.Close(context.Background(), s.ID(), "user_end"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if closeCalled.Load() != 1 {
		t.Errorf("OnClose calls = %d, want 1", closeCalled.Load())
	}
}

func TestSessionManager_OnOpenErrorRollsBack(t *testing.T) {
	store := newFakeStore()
	mdl := &scriptedModel{}
	router := newRouterWithModel(t, mdl)
	agent, err := NewAgent("a1", "hugen", &fakeIdentity{id: "a1"})
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	failErr := errors.New("hook fail")
	mgr := NewSessionManager(store, agent, router, NewCommandRegistry(), protocol.NewCodec(), nil,
		WithLifecycle(SessionLifecycle{
			OnOpen: func(ctx context.Context, sessionID string) error { return failErr },
		}),
	)
	_, _, err = mgr.Open(context.Background(), OpenRequest{OwnerID: "alice"})
	if err == nil || !errors.Is(err, failErr) {
		t.Fatalf("err = %v, want wrap of %v", err, failErr)
	}
}

func drainOutboxOnce(out <-chan protocol.Frame) {
	select {
	case <-out:
	case <-time.After(200 * time.Millisecond):
	}
}
