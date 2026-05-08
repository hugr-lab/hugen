package whiteboard

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/internal/fixture"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

const testAgentID = "agent-1"

func newExt() *Extension {
	return NewExtension(testAgentID)
}

// initState seeds a TestSessionState with a SessionWhiteboard handle
// the way the runtime would on session open.
func initState(t *testing.T, e *Extension, sid string) *fixture.TestSessionState {
	t.Helper()
	state := fixture.NewTestSessionState(sid)
	if err := e.InitState(context.Background(), state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	return state
}

func dispatchCtx(state extension.SessionState) context.Context {
	return extension.WithSessionState(context.Background(), state)
}

// TestList covers the static catalogue advertised by the
// ToolProvider — names, providers, permission objects.
func TestList(t *testing.T) {
	tools, err := newExt().List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := map[string]string{
		"whiteboard:init":  PermObjectWrite,
		"whiteboard:write": PermObjectWrite,
		"whiteboard:read":  PermObjectRead,
		"whiteboard:stop":  PermObjectWrite,
	}
	if len(tools) != len(want) {
		t.Fatalf("tools=%d, want %d", len(tools), len(want))
	}
	for _, tl := range tools {
		perm, ok := want[tl.Name]
		if !ok {
			t.Errorf("unexpected tool %q", tl.Name)
			continue
		}
		if tl.Provider != providerName {
			t.Errorf("provider for %s = %q, want %q", tl.Name, tl.Provider, providerName)
		}
		if tl.PermissionObject != perm {
			t.Errorf("permission for %s = %q, want %q", tl.Name, tl.PermissionObject, perm)
		}
	}
}

// TestCallInit_Happy: init on a fresh handle activates the
// projection and emits an ExtensionFrame{op:init}.
func TestCallInit_Happy(t *testing.T) {
	e := newExt()
	state := initState(t, e, "ses-1")
	ctx := dispatchCtx(state)

	out, err := e.Call(ctx, "whiteboard:init", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Call init: %v", err)
	}
	var got okOutput
	if err := json.Unmarshal(out, &got); err != nil || !got.OK {
		t.Fatalf("init output = %s err=%v", out, err)
	}
	if h := FromState(state); !h.Snapshot().Active {
		t.Errorf("handle not active after init")
	}
	emitted := state.Emitted()
	if len(emitted) != 1 {
		t.Fatalf("emitted=%d, want 1", len(emitted))
	}
	ef, ok := emitted[0].(*protocol.ExtensionFrame)
	if !ok {
		t.Fatalf("emitted type %T, want *ExtensionFrame", emitted[0])
	}
	if ef.Payload.Extension != providerName || ef.Payload.Op != OpInit {
		t.Errorf("emitted = %+v", ef.Payload)
	}
}

// TestCallInit_Idempotent: second init on an active board is a
// no-op — no extra event.
func TestCallInit_Idempotent(t *testing.T) {
	e := newExt()
	state := initState(t, e, "ses-1")
	ctx := dispatchCtx(state)
	if _, err := e.Call(ctx, "whiteboard:init", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := e.Call(ctx, "whiteboard:init", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("init #2: %v", err)
	}
	if got := len(state.Emitted()); got != 1 {
		t.Errorf("emitted=%d, want 1 (idempotent)", got)
	}
}

// TestCallWrite_NoParent: a root session writing surfaces
// no_whiteboard_to_write_to.
func TestCallWrite_NoParent(t *testing.T) {
	e := newExt()
	state := initState(t, e, "ses-root")
	ctx := dispatchCtx(state)
	args, _ := json.Marshal(writeInput{Text: "hi"})
	out, _ := e.Call(ctx, "whiteboard:write", args)
	assertErrorCode(t, out, "no_whiteboard_to_write_to")
}

// TestCallWrite_NoActiveHostBoard: a member writing while parent
// has no active board surfaces no_active_whiteboard.
func TestCallWrite_NoActiveHostBoard(t *testing.T) {
	e := newExt()
	parent := initState(t, e, "ses-parent")
	child := initState(t, e, "ses-child").WithParent(parent)
	parent.AppendChild(child)
	ctx := dispatchCtx(child)
	args, _ := json.Marshal(writeInput{Text: "hi"})
	out, _ := e.Call(ctx, "whiteboard:write", args)
	assertErrorCode(t, out, "no_active_whiteboard")
}

// TestCallWrite_BadRequest: missing-text refusal fires before
// presence / active checks.
func TestCallWrite_BadRequest(t *testing.T) {
	e := newExt()
	state := initState(t, e, "ses-1")
	ctx := dispatchCtx(state)
	out, _ := e.Call(ctx, "whiteboard:write", json.RawMessage(`{}`))
	assertErrorCode(t, out, "bad_request")
}

// TestCallWrite_Submits: a member's write Submits the op-frame to
// the host's inbox without persisting on the member.
func TestCallWrite_Submits(t *testing.T) {
	e := newExt()
	parent := initState(t, e, "ses-parent")
	child := initState(t, e, "ses-child").WithParent(parent)
	parent.AppendChild(child)

	// Activate parent's board manually via init so write can pass.
	if _, err := e.Call(dispatchCtx(parent), "whiteboard:init", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("init parent: %v", err)
	}

	args, _ := json.Marshal(writeInput{Text: "found x"})
	out, err := e.Call(dispatchCtx(child), "whiteboard:write", args)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !strings.Contains(string(out), `"ok":true`) {
		t.Fatalf("write output = %s", out)
	}
	if got := len(child.Emitted()); got != 0 {
		t.Errorf("child emitted=%d, want 0 (write only Submits to host)", got)
	}
	parentInbox := parent.Inbox()
	if len(parentInbox) != 1 {
		t.Fatalf("parent inbox=%d, want 1", len(parentInbox))
	}
	ef, ok := parentInbox[0].(*protocol.ExtensionFrame)
	if !ok {
		t.Fatalf("frame type %T", parentInbox[0])
	}
	if ef.Payload.Op != OpWrite || ef.Payload.Extension != providerName {
		t.Errorf("frame = %+v", ef.Payload)
	}
	if ef.FromSessionID() != "ses-child" {
		t.Errorf("from_session = %q, want ses-child", ef.FromSessionID())
	}
}

// TestCallRead_Inactive: fresh session reports active=false.
func TestCallRead_Inactive(t *testing.T) {
	e := newExt()
	state := initState(t, e, "ses-1")
	out, err := e.Call(dispatchCtx(state), "whiteboard:read", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got readOutput
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Active {
		t.Errorf("active = true on fresh session: %+v", got)
	}
}

// TestCallStop_Deactivates: stop after init flips Active=false and
// persists a stop op-frame.
func TestCallStop_Deactivates(t *testing.T) {
	e := newExt()
	state := initState(t, e, "ses-1")
	ctx := dispatchCtx(state)

	if _, err := e.Call(ctx, "whiteboard:init", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := e.Call(ctx, "whiteboard:stop", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if FromState(state).Snapshot().Active {
		t.Errorf("still active after stop")
	}
	stopFound := false
	for _, f := range state.Emitted() {
		ef, ok := f.(*protocol.ExtensionFrame)
		if !ok {
			continue
		}
		if ef.Payload.Op == OpStop {
			stopFound = true
		}
	}
	if !stopFound {
		t.Errorf("no whiteboard:stop frame emitted")
	}
}

// TestHandleFrame_HostInboundWrite: an inbound op:write on the host
// allocates seq, applies the projection, persists the canonical
// write event, and Submits a broadcast to every direct child
// (including the author).
func TestHandleFrame_HostInboundWrite(t *testing.T) {
	e := newExt()
	host := initState(t, e, "ses-host")
	a := initState(t, e, "ses-a").WithParent(host)
	b := initState(t, e, "ses-b").WithParent(host)
	host.AppendChild(a).AppendChild(b)

	if _, err := e.Call(dispatchCtx(host), "whiteboard:init", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("init: %v", err)
	}

	payload := writeData{
		FromSessionID: "ses-a",
		FromRole:      testAgentID,
		Text:          "found auth_logs",
	}
	raw, _ := json.Marshal(payload)
	inbound := protocol.NewExtensionFrame("ses-host", protocol.ParticipantInfo{ID: testAgentID, Kind: protocol.ParticipantAgent},
		providerName, protocol.CategoryOp, OpWrite, raw)
	inbound.BaseFrame.FromSession = "ses-a"

	if err := e.HandleFrame(dispatchCtx(host), host, inbound); err != nil {
		t.Fatalf("HandleFrame: %v", err)
	}

	hostWB := FromState(host).Snapshot()
	if len(hostWB.Messages) != 1 || hostWB.Messages[0].Seq != 1 {
		t.Errorf("host projection = %+v", hostWB)
	}
	if hostWB.Messages[0].FromSessionID != "ses-a" {
		t.Errorf("from_session_id = %q", hostWB.Messages[0].FromSessionID)
	}

	// Persisted canonical write frame on host.
	hostFrames := state_extensionFrames(host.Emitted(), OpWrite)
	if len(hostFrames) != 1 {
		t.Errorf("host write frames = %d, want 1", len(hostFrames))
	}

	// Broadcast Submitted to every child.
	for _, m := range []*fixture.TestSessionState{a, b} {
		inbox := m.Inbox()
		if len(inbox) != 1 {
			t.Errorf("%s inbox=%d, want 1", m.SessionID(), len(inbox))
			continue
		}
		ef, ok := inbox[0].(*protocol.ExtensionFrame)
		if !ok || ef.Payload.Op != OpMessage {
			t.Errorf("%s broadcast = %+v", m.SessionID(), inbox[0])
		}
	}
}

// TestHandleFrame_MemberBroadcast: an inbound op:message on a
// member persists a local write event AND Submits a SystemMessage
// back to the member's own inbox so the visibility filter folds it
// into history at the next turn boundary.
func TestHandleFrame_MemberBroadcast(t *testing.T) {
	e := newExt()
	member := initState(t, e, "ses-member")

	payload := writeData{
		Seq:           3,
		FromSessionID: "ses-author",
		FromRole:      "explorer",
		Text:          "ping",
	}
	raw, _ := json.Marshal(payload)
	bm := protocol.NewExtensionFrame("ses-member",
		protocol.ParticipantInfo{ID: testAgentID, Kind: protocol.ParticipantAgent},
		providerName, protocol.CategoryMessage, OpMessage, raw)
	bm.BaseFrame.FromSession = "ses-host"

	if err := e.HandleFrame(dispatchCtx(member), member, bm); err != nil {
		t.Fatalf("HandleFrame: %v", err)
	}

	wb := FromState(member).Snapshot()
	if !wb.Active || len(wb.Messages) != 1 || wb.Messages[0].Seq != 3 {
		t.Errorf("member projection = %+v", wb)
	}

	writeFrames := state_extensionFrames(member.Emitted(), OpWrite)
	if len(writeFrames) != 1 {
		t.Errorf("local write frames = %d, want 1", len(writeFrames))
	}

	// Synthetic init must be persisted ahead of the write so
	// Recovery on restart can rebuild the projection (Apply drops
	// writes against an inactive whiteboard). Init+write order in
	// the emitted log is load-bearing.
	initFrames := state_extensionFrames(member.Emitted(), OpInit)
	if len(initFrames) != 1 {
		t.Errorf("synthetic init frames = %d, want 1", len(initFrames))
	}
	emitted := member.Emitted()
	if len(emitted) >= 2 {
		first, _ := emitted[0].(*protocol.ExtensionFrame)
		second, _ := emitted[1].(*protocol.ExtensionFrame)
		if first == nil || first.Payload.Op != OpInit ||
			second == nil || second.Payload.Op != OpWrite {
			t.Errorf("emit order = %+v / %+v, want init→write",
				first.Payload, second.Payload)
		}
	}

	inbox := member.Inbox()
	if len(inbox) != 1 {
		t.Fatalf("self-inbox = %d, want 1 (system_message routed back)", len(inbox))
	}
	sm, ok := inbox[0].(*protocol.SystemMessage)
	if !ok || sm.Payload.Kind != protocol.SystemMessageWhiteboard {
		t.Errorf("self-inbox frame = %+v", inbox[0])
	}
	if !strings.Contains(sm.Payload.Content, "explorer") || !strings.Contains(sm.Payload.Content, "ping") {
		t.Errorf("system_message content = %q", sm.Payload.Content)
	}
}

// TestRecover_RebuildsProjection: Recover walks the persisted
// extension_frame events for the whiteboard ext and rebuilds the
// full projection without any tool-path side effects.
func TestRecover_RebuildsProjection(t *testing.T) {
	e := newExt()
	state := initState(t, e, "ses-1")

	at := time.Now().UTC()
	rows := []store.EventRow{
		{
			SessionID: "ses-1",
			EventType: string(protocol.KindExtensionFrame),
			CreatedAt: at,
			Metadata: map[string]any{
				"extension": providerName,
				"category":  string(protocol.CategoryOp),
				"op":        OpInit,
			},
		},
		{
			SessionID: "ses-1",
			EventType: string(protocol.KindExtensionFrame),
			CreatedAt: at.Add(time.Second),
			Metadata: map[string]any{
				"extension": providerName,
				"category":  string(protocol.CategoryOp),
				"op":        OpWrite,
				"data": map[string]any{
					"seq":             float64(1),
					"from_session_id": "ses-a",
					"from_role":       "explorer",
					"text":            "found x",
				},
			},
		},
	}
	if err := e.Recover(context.Background(), state, rows); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	wb := FromState(state).Snapshot()
	if !wb.Active || len(wb.Messages) != 1 {
		t.Fatalf("projection = %+v", wb)
	}
	if wb.Messages[0].Text != "found x" || wb.Messages[0].Seq != 1 {
		t.Errorf("message = %+v", wb.Messages[0])
	}
}

func assertErrorCode(t *testing.T, raw json.RawMessage, want string) {
	t.Helper()
	var got toolErrorResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal err response: %v (raw=%s)", err, raw)
	}
	if got.Error.Code != want {
		t.Errorf("error code = %q, want %q (raw=%s)", got.Error.Code, want, raw)
	}
}

// state_extensionFrames extracts ExtensionFrames from an emitted
// list whose Op matches the requested op. Helper for tests.
func state_extensionFrames(emitted []protocol.Frame, op string) []*protocol.ExtensionFrame {
	var out []*protocol.ExtensionFrame
	for _, f := range emitted {
		ef, ok := f.(*protocol.ExtensionFrame)
		if !ok {
			continue
		}
		if ef.Payload.Op == op {
			out = append(out, ef)
		}
	}
	return out
}
