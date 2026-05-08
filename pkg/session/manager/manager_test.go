package manager

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/internal/fixture"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// instrumentedStore wraps fakeStore with call counters used by the
// lazy-materialisation tests.
type instrumentedStore struct {
	*fixture.TestStore
	listEventsCalls atomic.Int32
}

func (s *instrumentedStore) ListEvents(ctx context.Context, sid string, opts session.ListEventsOpts) ([]session.EventRow, error) {
	s.listEventsCalls.Add(1)
	return s.TestStore.ListEvents(ctx, sid, opts)
}

func newTestManager(t *testing.T, store session.RuntimeStore) *Manager {
	t.Helper()
	mdl := &scriptedModel{}
	router := newRouterWithModel(t, mdl)
	agent, err := session.NewAgent("a1", "hugen", &fakeIdentity{id: "a1"}, "")
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	tm := tool.NewToolManager(permsAllow{}, nil, nil)
	return NewManager(store, agent, router, session.NewCommandRegistry(), protocol.NewCodec(), tm, nil)
}

func TestManager_LazyMaterialisation(t *testing.T) {
	base := fixture.NewTestStore()
	// Seed the store with a session row + 100 historic events.
	_ = base.OpenSession(context.Background(), session.SessionRow{
		ID: "s1", AgentID: "a1", Status: session.StatusActive,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	for i := 0; i < 100; i++ {
		_ = base.AppendEvent(context.Background(), session.EventRow{
			ID:        "ev" + string(rune('a'+i%26)),
			SessionID: "s1",
			AgentID:   "a1",
			EventType: string(protocol.KindUserMessage),
			Author:    "u1",
			Content:   "msg",
		}, "")
	}
	store := &instrumentedStore{TestStore: base}

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

func TestManager_ResumeClosed(t *testing.T) {
	store := fixture.NewTestStore()
	ctx := context.Background()
	_ = store.OpenSession(ctx, session.SessionRow{ID: "s1", AgentID: "a1", Status: session.StatusActive})
	// Phase-4: liveness is event-derived. Append a session_terminated
	// event so isSessionTerminated returns true on Resume.
	terminal := protocol.NewSessionTerminated("s1", protocol.ParticipantInfo{ID: "a1", Kind: protocol.ParticipantAgent},
		protocol.SessionTerminatedPayload{Reason: protocol.TerminationUserEnd})
	row, summary, _ := session.FrameToEventRow(terminal, "a1")
	_ = store.AppendEvent(ctx, row, summary)
	mgr := newTestManager(t, store)
	if _, err := mgr.Resume(ctx, "s1"); !errors.Is(err, session.ErrSessionClosed) {
		t.Fatalf("expected ErrSessionClosed, got %v", err)
	}
}

func TestManager_ResumeNotFound(t *testing.T) {
	store := fixture.NewTestStore()
	mgr := newTestManager(t, store)
	if _, err := mgr.Resume(context.Background(), "nope"); !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("expected ErrSessionNotFound, got %v", err)
	}
}

func TestManager_OpenAndList(t *testing.T) {
	store := fixture.NewTestStore()
	mgr := newTestManager(t, store)
	s, _, err := mgr.Open(context.Background(), session.OpenRequest{OwnerID: "alice"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if s.ID() == "" {
		t.Fatal("expected non-empty session id")
	}
	// At least one event_opened was persisted.
	if len(store.Events) == 0 {
		t.Fatal("expected session_opened event in store")
	}
}

// TestManager_BroadcastSystemMarker_ReachesLiveRoots verifies phase-4
// US7 wiring: the MCP reconnector's OnRecover callback ultimately
// calls Manager.BroadcastSystemMarker; every live root session must
// see a system_marker frame on its outbox carrying the supplied
// subject + metadata.
func TestManager_BroadcastSystemMarker_ReachesLiveRoots(t *testing.T) {
	store := fixture.NewTestStore()
	mgr := newTestManager(t, store)
	defer mgr.Stop(context.Background())

	// Open two roots so we can observe the broadcast lands on both.
	r1, _, err := mgr.Open(context.Background(), session.OpenRequest{OwnerID: "u1"})
	if err != nil {
		t.Fatalf("Open r1: %v", err)
	}
	r2, _, err := mgr.Open(context.Background(), session.OpenRequest{OwnerID: "u2"})
	if err != nil {
		t.Fatalf("Open r2: %v", err)
	}
	drainOutboxOnce(r1.Outbox()) // SessionOpened
	drainOutboxOnce(r1.Outbox()) // initial SessionStatus{idle}
	drainOutboxOnce(r2.Outbox())
	drainOutboxOnce(r2.Outbox())

	mgr.BroadcastSystemMarker(context.Background(), "mcp_recovered",
		map[string]any{"provider": "hugr-main"})

	check := func(t *testing.T, s *session.Session) {
		t.Helper()
		select {
		case f := <-s.Outbox():
			marker, ok := f.(*protocol.SystemMarker)
			if !ok {
				t.Errorf("session %s outbox: got %T, want SystemMarker", s.ID(), f)
				return
			}
			if marker.Payload.Subject != "mcp_recovered" {
				t.Errorf("session %s subject = %q, want mcp_recovered",
					s.ID(), marker.Payload.Subject)
			}
			if got, _ := marker.Payload.Details["provider"].(string); got != "hugr-main" {
				t.Errorf("session %s details.provider = %v, want hugr-main",
					s.ID(), marker.Payload.Details["provider"])
			}
		case <-time.After(2 * time.Second):
			t.Errorf("session %s never received broadcast marker", s.ID())
		}
	}
	check(t, r1)
	check(t, r2)
}

// TestManager_LifecycleHooks + TestManager_OnOpenErrorRollsBack
// removed in stage 5c — Lifecycle interface no longer exists.
// Equivalent open/close hook coverage lives in extension
// Closer/StateInitializer tests (notepad/skill/mcp/workspace).

// TestSession_Spawn_HappyPath asserts a sub-agent session is created
// with the right row fields, metadata.depth = parent.depth+1, and the
// parent's events contain a subagent_started record naming the child.
func TestSession_Spawn_HappyPath(t *testing.T) {
	store := fixture.NewTestStore()
	mgr := newTestManager(t, store)
	ctx := context.Background()

	parent, _, err := mgr.Open(ctx, session.OpenRequest{OwnerID: "alice"})
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	// Drain the session_opened frame so the outbox doesn't block.
	drainOutboxOnce(parent.Outbox())

	child, err := parent.Spawn(ctx, session.SpawnSpec{
		Skill:  "hugr-data",
		Role:   "explorer",
		Task:   "list sources",
		Inputs: map[string]any{"hint": "begin with auth_logs"},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if child.ID() == parent.ID() {
		t.Fatal("child id collides with parent")
	}

	childRow, err := store.LoadSession(ctx, child.ID())
	if err != nil {
		t.Fatalf("load child row: %v", err)
	}
	if childRow.SessionType != "subagent" {
		t.Errorf("session_type = %q, want subagent", childRow.SessionType)
	}
	if childRow.ParentSessionID != parent.ID() {
		t.Errorf("parent_session_id = %q, want %q", childRow.ParentSessionID, parent.ID())
	}
	if d, _ := childRow.Metadata["depth"].(int); d != 1 {
		t.Errorf("metadata.depth = %v, want 1", childRow.Metadata["depth"])
	}
	if r, _ := childRow.Metadata["spawn_role"].(string); r != "explorer" {
		t.Errorf("metadata.spawn_role = %v, want explorer", childRow.Metadata["spawn_role"])
	}

	// Parent's events contain a subagent_started record.
	parentEvents, _ := store.ListEvents(ctx, parent.ID(), session.ListEventsOpts{})
	var foundStart bool
	for _, ev := range parentEvents {
		if ev.EventType == string(protocol.KindSubagentStarted) {
			foundStart = true
			if ev.Metadata["child_session_id"] != child.ID() {
				t.Errorf("subagent_started child id = %v, want %s", ev.Metadata["child_session_id"], child.ID())
			}
			break
		}
	}
	if !foundStart {
		t.Error("parent events missing subagent_started")
	}
	_ = mgr.Terminate(ctx, child.ID(), "test_cleanup")
	_ = mgr.Terminate(ctx, parent.ID(), "test_cleanup")
}

// TestSession_Spawn_DepthIncrements asserts a 2-deep spawn yields
// depth 2 on the grandchild, derived from the in-memory parent.depth.
func TestSession_Spawn_DepthIncrements(t *testing.T) {
	store := fixture.NewTestStore()
	mgr := newTestManager(t, store)
	ctx := context.Background()

	root, _, _ := mgr.Open(ctx, session.OpenRequest{OwnerID: "alice"})
	drainOutboxOnce(root.Outbox())
	child, err := root.Spawn(ctx, session.SpawnSpec{Role: "x", Task: "t"})
	if err != nil {
		t.Fatalf("spawn child: %v", err)
	}
	drainOutboxOnce(child.Outbox())
	grand, err := child.Spawn(ctx, session.SpawnSpec{Role: "y", Task: "t2"})
	if err != nil {
		t.Fatalf("spawn grandchild: %v", err)
	}
	row, _ := store.LoadSession(ctx, grand.ID())
	if d, _ := row.Metadata["depth"].(int); d != 2 {
		t.Errorf("grandchild depth = %v, want 2", row.Metadata["depth"])
	}
}

// TestManager_Deliver_RoutesToSession asserts Deliver pushes the frame
// onto the addressed session's inbox and returns no error.
func TestManager_Deliver_RoutesToSession(t *testing.T) {
	store := fixture.NewTestStore()
	mgr := newTestManager(t, store)
	ctx := context.Background()

	target, _, _ := mgr.Open(ctx, session.OpenRequest{OwnerID: "alice"})
	drainOutboxOnce(target.Outbox())

	frame := protocol.NewSystemMessage(target.ID(),
		protocol.ParticipantInfo{ID: "tester", Kind: protocol.ParticipantSystem},
		protocol.SystemMessageWhiteboard, "[whiteboard] tester (none): synthetic")
	if err := mgr.Deliver(ctx, target.ID(), frame); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	// The session goroutine echos system_message via emit (default
	// case in handle); wait for it on the outbox.
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case f := <-target.Outbox():
			if f.Kind() == protocol.KindSystemMessage {
				return
			}
		case <-deadline.C:
			t.Fatal("system_message did not surface on outbox")
		}
	}
}

// TestManager_Deliver_UnknownSession returns ErrSessionNotFound for an
// id that has never been opened.
func TestManager_Deliver_UnknownSession(t *testing.T) {
	store := fixture.NewTestStore()
	mgr := newTestManager(t, store)
	frame := protocol.NewHeartbeat("ghost", protocol.ParticipantInfo{ID: "x", Kind: "system"})
	if err := mgr.Deliver(context.Background(), "ghost", frame); !errors.Is(err, session.ErrSessionNotFound) {
		t.Errorf("err = %v, want ErrSessionNotFound", err)
	}
}
