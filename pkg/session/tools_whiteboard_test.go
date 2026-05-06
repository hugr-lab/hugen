package session

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/whiteboard"
)

// ---------- whiteboard_init ----------

// TestCallWhiteboardInit_Happy: a fresh session activates a board;
// the in-memory projection becomes Active and a whiteboard_op{op:"init"}
// event lands in the store.
func TestCallWhiteboardInit_Happy(t *testing.T) {
	store := newFakeStore()
	mgr := newTestManager(t, store)
	defer mgr.Stop(context.Background())
	parent := us1OpenParent(t, mgr)

	out, err := callWhiteboardInit(us1WithSession(parent), parent, mgrToolHost(mgr), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var got whiteboardOKOutput
	if err := json.Unmarshal(out, &got); err != nil || !got.OK {
		t.Fatalf("init output = %s err=%v", out, err)
	}

	parent.whiteboardMu.Lock()
	wb := parent.whiteboard
	parent.whiteboardMu.Unlock()
	if !wb.Active {
		t.Errorf("in-memory board not active after init")
	}

	events, _ := store.ListEvents(context.Background(), parent.id, ListEventsOpts{})
	found := false
	for _, ev := range events {
		if ev.EventType == string(protocol.KindWhiteboardOp) && ev.Metadata["op"] == "init" {
			found = true
		}
	}
	if !found {
		t.Errorf("no whiteboard_op{init} persisted; events=%v", kindsOnly(events))
	}
}

// TestCallWhiteboardInit_Idempotent: a second init on an active board
// returns ok with no second event written.
func TestCallWhiteboardInit_Idempotent(t *testing.T) {
	store := newFakeStore()
	mgr := newTestManager(t, store)
	defer mgr.Stop(context.Background())
	parent := us1OpenParent(t, mgr)

	if _, err := callWhiteboardInit(us1WithSession(parent), parent, mgrToolHost(mgr), json.RawMessage(`{}`)); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := callWhiteboardInit(us1WithSession(parent), parent, mgrToolHost(mgr), json.RawMessage(`{}`)); err != nil {
		t.Fatalf("second init: %v", err)
	}
	events, _ := store.ListEvents(context.Background(), parent.id, ListEventsOpts{})
	count := 0
	for _, ev := range events {
		if ev.EventType == string(protocol.KindWhiteboardOp) && ev.Metadata["op"] == "init" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("init events = %d, want 1 (idempotent)", count)
	}
}

// ---------- whiteboard_write ----------

// TestCallWhiteboardWrite_NoParent: a root session writing surfaces
// the no_whiteboard_to_write_to refusal.
func TestCallWhiteboardWrite_NoParent(t *testing.T) {
	mgr := newTestManager(t, newFakeStore())
	defer mgr.Stop(context.Background())
	parent := us1OpenParent(t, mgr)

	args, _ := json.Marshal(whiteboardWriteInput{Text: "hi"})
	out, _ := callWhiteboardWrite(us1WithSession(parent), parent, mgrToolHost(mgr), args)
	mgr_assertErrorCode(t, out, "no_whiteboard_to_write_to")
}

// TestCallWhiteboardWrite_NoActiveBoard: a sub-agent whose parent has
// no active board sees no_active_whiteboard.
func TestCallWhiteboardWrite_NoActiveBoard(t *testing.T) {
	mgr := newTestManager(t, newFakeStore())
	ctx := context.Background()
	defer mgr.Stop(ctx)

	parent := us1OpenParent(t, mgr)
	child, err := parent.Spawn(ctx, SpawnSpec{Task: "x"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	drainOutboxOnce(parent.Outbox())

	args, _ := json.Marshal(whiteboardWriteInput{Text: "ping"})
	out, _ := callWhiteboardWrite(us1WithSession(child), child, mgrToolHost(mgr), args)
	mgr_assertErrorCode(t, out, "no_active_whiteboard")
}

// TestCallWhiteboardWrite_BadRequest: missing-text refusal fires
// before the parent-presence / active-board checks.
func TestCallWhiteboardWrite_BadRequest(t *testing.T) {
	mgr := newTestManager(t, newFakeStore())
	defer mgr.Stop(context.Background())
	parent := us1OpenParent(t, mgr)

	out, _ := callWhiteboardWrite(us1WithSession(parent), parent, mgrToolHost(mgr), json.RawMessage(`{}`))
	mgr_assertErrorCode(t, out, "bad_request")
}

// ---------- whiteboard_read ----------

// TestCallWhiteboardRead_Inactive: a fresh session returns active=false.
func TestCallWhiteboardRead_Inactive(t *testing.T) {
	mgr := newTestManager(t, newFakeStore())
	defer mgr.Stop(context.Background())
	parent := us1OpenParent(t, mgr)

	out, err := callWhiteboardRead(us1WithSession(parent), parent, mgrToolHost(mgr), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got whiteboardReadOutput
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Active {
		t.Errorf("expected active=false on fresh session, got %+v", got)
	}
}

// TestCallWhiteboardRead_OwnHostedAfterInit: after init the read
// returns the host's projection (no messages yet, but Active=true).
func TestCallWhiteboardRead_OwnHostedAfterInit(t *testing.T) {
	mgr := newTestManager(t, newFakeStore())
	defer mgr.Stop(context.Background())
	parent := us1OpenParent(t, mgr)

	if _, err := callWhiteboardInit(us1WithSession(parent), parent, mgrToolHost(mgr), json.RawMessage(`{}`)); err != nil {
		t.Fatalf("init: %v", err)
	}
	out, _ := callWhiteboardRead(us1WithSession(parent), parent, mgrToolHost(mgr), json.RawMessage(`{}`))
	var got whiteboardReadOutput
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Active {
		t.Errorf("read should report active=true after init, got %+v", got)
	}
	if got.HostID != parent.id {
		t.Errorf("HostID = %q, want %q (own hosted)", got.HostID, parent.id)
	}
}

// ---------- whiteboard_stop ----------

// TestCallWhiteboardStop_DeactivatesProjection: stop after init flips
// Active=false and writes a stop event.
func TestCallWhiteboardStop_DeactivatesProjection(t *testing.T) {
	store := newFakeStore()
	mgr := newTestManager(t, store)
	defer mgr.Stop(context.Background())
	parent := us1OpenParent(t, mgr)

	_, _ = callWhiteboardInit(us1WithSession(parent), parent, mgrToolHost(mgr), json.RawMessage(`{}`))
	if _, err := callWhiteboardStop(us1WithSession(parent), parent, mgrToolHost(mgr), json.RawMessage(`{}`)); err != nil {
		t.Fatalf("stop: %v", err)
	}

	parent.whiteboardMu.Lock()
	if parent.whiteboard.Active {
		t.Errorf("board still active after stop: %+v", parent.whiteboard)
	}
	parent.whiteboardMu.Unlock()

	events, _ := store.ListEvents(context.Background(), parent.id, ListEventsOpts{})
	stopFound := false
	for _, ev := range events {
		if ev.EventType == string(protocol.KindWhiteboardOp) && ev.Metadata["op"] == "stop" {
			stopFound = true
		}
	}
	if !stopFound {
		t.Errorf("no whiteboard_op{stop} persisted; events=%v", kindsOnly(events))
	}
}

// TestCallWhiteboardStop_OnInactive: stop with no prior init is a
// no-op success — idempotent, no event.
func TestCallWhiteboardStop_OnInactive(t *testing.T) {
	store := newFakeStore()
	mgr := newTestManager(t, store)
	defer mgr.Stop(context.Background())
	parent := us1OpenParent(t, mgr)

	if _, err := callWhiteboardStop(us1WithSession(parent), parent, mgrToolHost(mgr), json.RawMessage(`{}`)); err != nil {
		t.Fatalf("stop: %v", err)
	}
	events, _ := store.ListEvents(context.Background(), parent.id, ListEventsOpts{})
	for _, ev := range events {
		if ev.EventType == string(protocol.KindWhiteboardOp) {
			t.Errorf("unexpected whiteboard_op event for stop on inactive board: %+v", ev.Metadata)
		}
	}
}

// ---------- closed-session dispatch ----------

func TestCallWhiteboard_SessionGone(t *testing.T) {
	mgr := newTestManager(t, newFakeStore())
	defer mgr.Stop(context.Background())
	parent := us1OpenParent(t, mgr)
	parent.closed.Store(true)

	for name, call := range map[string]sessionToolHandler{
		"init":  callWhiteboardInit,
		"write": callWhiteboardWrite,
		"read":  callWhiteboardRead,
		"stop":  callWhiteboardStop,
	} {
		out, err := call(us1WithSession(parent), parent, mgrToolHost(mgr), json.RawMessage(`{"text":"x"}`))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		mgr_assertErrorCode(t, out, "session_gone")
	}
}

// kindsOnly is a debug helper for failing assertions — extracts the
// sequence of event kinds for a more readable error message.
func kindsOnly(rows []EventRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.EventType)
	}
	return out
}

// _ ensures the projection import is referenced even if all tests
// drift to using only the protocol's WhiteboardOpPayload directly.
var _ = whiteboard.OpInit
