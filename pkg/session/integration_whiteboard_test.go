package session

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/whiteboard"
)

// TestUS3_Whiteboard_BroadcastEndToEnd is the canonical phase-4-spec
// §13.2 #14 / #15 path: a parent inits a board, spawns two children,
// one child writes, and:
//
//   - the host persists a whiteboard_op{op:"write", seq=1} in its own
//     events;
//   - the host's in-memory projection contains exactly one message
//     with the right text + author;
//   - both members (author + sibling) receive a whiteboard_message
//     broadcast that the member-side internal handler converts into
//     a local whiteboard_op + system_message + history line.
func TestUS3_Whiteboard_BroadcastEndToEnd(t *testing.T) {
	store := newFakeStore()
	mgr := newTestManager(t, store)
	ctx := context.Background()
	defer mgr.ShutdownAll(ctx)

	parent := us1OpenParent(t, mgr)

	// 1. Host opens the board.
	if _, err := callWhiteboardInit(us1WithSession(parent), mgr, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("init: %v", err)
	}

	// 2. Spawn two children.
	childA, err := parent.Spawn(ctx, SpawnSpec{Task: "scout-a"})
	if err != nil {
		t.Fatalf("spawn A: %v", err)
	}
	childB, err := parent.Spawn(ctx, SpawnSpec{Task: "scout-b"})
	if err != nil {
		t.Fatalf("spawn B: %v", err)
	}
	drainOutboxOnce(parent.Outbox()) // subagent_started{a}
	drainOutboxOnce(parent.Outbox()) // subagent_started{b}
	drainOutboxOnce(childA.Outbox())
	drainOutboxOnce(childB.Outbox())

	// 3. Child A writes a broadcast.
	args, _ := json.Marshal(whiteboardWriteInput{Text: "found auth_logs"})
	out, err := callWhiteboardWrite(us1WithSession(childA), mgr, args)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !strings.Contains(string(out), `"ok":true`) {
		t.Fatalf("write output = %s", out)
	}

	// 4. Wait for the host's RouteInternal handler to land the
	// whiteboard_op + Submit broadcasts to both members; then for
	// each member's RouteInternal handler to land its local copy.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		parent.whiteboardMu.Lock()
		hostHas := len(parent.whiteboard.Messages) == 1
		parent.whiteboardMu.Unlock()
		childA.whiteboardMu.Lock()
		aHas := len(childA.whiteboard.Messages) == 1
		childA.whiteboardMu.Unlock()
		childB.whiteboardMu.Lock()
		bHas := len(childB.whiteboard.Messages) == 1
		childB.whiteboardMu.Unlock()
		if hostHas && aHas && bHas {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	parent.whiteboardMu.Lock()
	hostMsgs := append([]whiteboard.Message{}, parent.whiteboard.Messages...)
	parent.whiteboardMu.Unlock()
	if len(hostMsgs) != 1 {
		t.Fatalf("host projection has %d messages, want 1", len(hostMsgs))
	}
	if hostMsgs[0].Seq != 1 || hostMsgs[0].Text != "found auth_logs" {
		t.Errorf("host msg = %+v, want seq=1 text=found auth_logs", hostMsgs[0])
	}
	if hostMsgs[0].FromSessionID != childA.id {
		t.Errorf("host msg author = %q, want %q", hostMsgs[0].FromSessionID, childA.id)
	}

	// Each member should have one whiteboard_op{op:"write"} in its
	// own events (the local copy persisted by the member-side handler)
	// and one system_message{kind:"whiteboard"} for model visibility.
	for _, m := range []*Session{childA, childB} {
		evs, _ := store.ListEvents(ctx, m.id, ListEventsOpts{})
		opCount := 0
		smCount := 0
		for _, ev := range evs {
			if ev.EventType == string(protocol.KindWhiteboardOp) && ev.Metadata["op"] == "write" {
				opCount++
			}
			if ev.EventType == string(protocol.KindSystemMessage) && ev.Metadata["kind"] == "whiteboard" {
				smCount++
			}
		}
		if opCount != 1 {
			t.Errorf("session %q whiteboard_op{write} count = %d, want 1", m.id, opCount)
		}
		if smCount != 1 {
			t.Errorf("session %q system_message{whiteboard} count = %d, want 1", m.id, smCount)
		}
	}

	// Host should have the canonical write event too.
	evs, _ := store.ListEvents(ctx, parent.id, ListEventsOpts{})
	hostWriteFound := false
	for _, ev := range evs {
		if ev.EventType == string(protocol.KindWhiteboardOp) && ev.Metadata["op"] == "write" {
			hostWriteFound = true
		}
	}
	if !hostWriteFound {
		t.Errorf("host did not persist canonical whiteboard_op{write}")
	}
}

// TestUS3_Whiteboard_StopRefusesNewWrites pins the spec contract:
// after whiteboard_stop, a member's write surfaces no_active_whiteboard
// rather than silently dropping.
func TestUS3_Whiteboard_StopRefusesNewWrites(t *testing.T) {
	mgr := newTestManager(t, newFakeStore())
	ctx := context.Background()
	defer mgr.ShutdownAll(ctx)

	parent := us1OpenParent(t, mgr)
	_, _ = callWhiteboardInit(us1WithSession(parent), mgr, json.RawMessage(`{}`))

	child, err := parent.Spawn(ctx, SpawnSpec{Task: "x"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	drainOutboxOnce(parent.Outbox())

	if _, err := callWhiteboardStop(us1WithSession(parent), mgr, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("stop: %v", err)
	}

	args, _ := json.Marshal(whiteboardWriteInput{Text: "too late"})
	out, _ := callWhiteboardWrite(us1WithSession(child), mgr, args)
	mgr_assertErrorCode(t, out, "no_active_whiteboard")
}

// TestUS3_Whiteboard_SurvivesRestart exercises the §7.6 promise: the
// host's projection rebuilds entirely from whiteboard_op events on
// resume. Boot1 inits a board + writes via a child + stops; Boot2
// opens a fresh Manager against the same store, resumes the host, and
// asserts the projection reflects the full event log.
func TestUS3_Whiteboard_SurvivesRestart(t *testing.T) {
	store := newFakeStore()
	ctx := context.Background()

	// Boot 1.
	mgr1 := newTestManager(t, store)
	parent1, _, err := mgr1.Open(ctx, OpenRequest{OwnerID: "alice"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	drainOutboxOnce(parent1.Outbox())

	if _, err := callWhiteboardInit(us1WithSession(parent1), mgr1, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("init: %v", err)
	}
	child, err := parent1.Spawn(ctx, SpawnSpec{Task: "x"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	drainOutboxOnce(parent1.Outbox())

	args, _ := json.Marshal(whiteboardWriteInput{Text: "anchor msg"})
	if _, err := callWhiteboardWrite(us1WithSession(child), mgr1, args); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Wait for host RouteInternal to land the canonical write event.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		parent1.whiteboardMu.Lock()
		got := len(parent1.whiteboard.Messages)
		parent1.whiteboardMu.Unlock()
		if got == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	parentID := parent1.id
	mgr1.ShutdownAll(ctx) // graceful — writes nothing terminal.

	// Boot 2: fresh Manager, same store.
	mgr2 := newTestManager(t, store)
	defer mgr2.ShutdownAll(ctx)

	resumed, err := mgr2.Resume(ctx, parentID)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if err := resumed.materialise(ctx); err != nil {
		t.Fatalf("materialise: %v", err)
	}

	resumed.whiteboardMu.Lock()
	wb := resumed.whiteboard
	resumed.whiteboardMu.Unlock()
	if !wb.Active {
		t.Errorf("resumed host whiteboard not active: %+v", wb)
	}
	if len(wb.Messages) != 1 || wb.Messages[0].Text != "anchor msg" {
		t.Errorf("resumed host messages = %+v, want one 'anchor msg'", wb.Messages)
	}
	if wb.NextSeq != 2 {
		t.Errorf("NextSeq = %d, want 2 after one write", wb.NextSeq)
	}
}
