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
	parent, cleanup := newTestParent(t, withTestStore(store))
	defer cleanup()

	out, err := parent.callWhiteboardInit(us1WithSession(parent), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var got whiteboardOKOutput
	if err := json.Unmarshal(out, &got); err != nil || !got.OK {
		t.Fatalf("init output = %s err=%v", out, err)
	}

	wb := parent.WhiteboardSnapshot()
	if !wb.Active {
		t.Errorf("in-memory board not active after init")
	}

	events, _ := store.ListEvents(context.Background(), parent.ID(), ListEventsOpts{})
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
	parent, cleanup := newTestParent(t, withTestStore(store))
	defer cleanup()

	if _, err := parent.callWhiteboardInit(us1WithSession(parent), json.RawMessage(`{}`)); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := parent.callWhiteboardInit(us1WithSession(parent), json.RawMessage(`{}`)); err != nil {
		t.Fatalf("second init: %v", err)
	}
	events, _ := store.ListEvents(context.Background(), parent.ID(), ListEventsOpts{})
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
	parent, cleanup := newTestParent(t)
	defer cleanup()

	args, _ := json.Marshal(whiteboardWriteInput{Text: "hi"})
	out, _ := parent.callWhiteboardWrite(us1WithSession(parent), args)
	mgr_assertErrorCode(t, out, "no_whiteboard_to_write_to")
}

// TestCallWhiteboardWrite_NoActiveBoard: a sub-agent whose parent has
// no active board sees no_active_whiteboard.
func TestCallWhiteboardWrite_NoActiveBoard(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	ctx := context.Background()
	child, err := parent.Spawn(ctx, SpawnSpec{Task: "x"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	drainOutboxOnce(parent.Outbox())

	args, _ := json.Marshal(whiteboardWriteInput{Text: "ping"})
	out, _ := child.callWhiteboardWrite(us1WithSession(child), args)
	mgr_assertErrorCode(t, out, "no_active_whiteboard")
}

// TestCallWhiteboardWrite_BadRequest: missing-text refusal fires
// before the parent-presence / active-board checks.
func TestCallWhiteboardWrite_BadRequest(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	out, _ := parent.callWhiteboardWrite(us1WithSession(parent), json.RawMessage(`{}`))
	mgr_assertErrorCode(t, out, "bad_request")
}

// ---------- whiteboard_read ----------

// TestCallWhiteboardRead_Inactive: a fresh session returns active=false.
func TestCallWhiteboardRead_Inactive(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	out, err := parent.callWhiteboardRead(us1WithSession(parent), json.RawMessage(`{}`))
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
	parent, cleanup := newTestParent(t)
	defer cleanup()

	if _, err := parent.callWhiteboardInit(us1WithSession(parent), json.RawMessage(`{}`)); err != nil {
		t.Fatalf("init: %v", err)
	}
	out, _ := parent.callWhiteboardRead(us1WithSession(parent), json.RawMessage(`{}`))
	var got whiteboardReadOutput
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Active {
		t.Errorf("read should report active=true after init, got %+v", got)
	}
	if got.HostID != parent.ID() {
		t.Errorf("HostID = %q, want %q (own hosted)", got.HostID, parent.ID())
	}
}

// ---------- whiteboard_stop ----------

// TestCallWhiteboardStop_DeactivatesProjection: stop after init flips
// Active=false and writes a stop event.
func TestCallWhiteboardStop_DeactivatesProjection(t *testing.T) {
	store := newFakeStore()
	parent, cleanup := newTestParent(t, withTestStore(store))
	defer cleanup()

	_, _ = parent.callWhiteboardInit(us1WithSession(parent), json.RawMessage(`{}`))
	if _, err := parent.callWhiteboardStop(us1WithSession(parent), json.RawMessage(`{}`)); err != nil {
		t.Fatalf("stop: %v", err)
	}

	if parent.WhiteboardSnapshot().Active {
		t.Errorf("board still active after stop: %+v", parent.WhiteboardSnapshot())
	}

	events, _ := store.ListEvents(context.Background(), parent.ID(), ListEventsOpts{})
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
	parent, cleanup := newTestParent(t, withTestStore(store))
	defer cleanup()

	if _, err := parent.callWhiteboardStop(us1WithSession(parent), json.RawMessage(`{}`)); err != nil {
		t.Fatalf("stop: %v", err)
	}
	events, _ := store.ListEvents(context.Background(), parent.ID(), ListEventsOpts{})
	for _, ev := range events {
		if ev.EventType == string(protocol.KindWhiteboardOp) {
			t.Errorf("unexpected whiteboard_op event for stop on inactive board: %+v", ev.Metadata)
		}
	}
}

// ---------- closed-session dispatch ----------

func TestCallWhiteboard_SessionGone(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()
	parent.MarkClosed()

	for name, call := range map[string]sessionToolHandler{
		"init":  (*Session).callWhiteboardInit,
		"write": (*Session).callWhiteboardWrite,
		"read":  (*Session).callWhiteboardRead,
		"stop":  (*Session).callWhiteboardStop,
	} {
		out, err := call(parent, us1WithSession(parent), json.RawMessage(`{"text":"x"}`))
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
