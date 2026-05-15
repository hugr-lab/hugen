package liveview

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/internal/fixture"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// TestExtension_InitState_AllocatesAndSpawns covers the happy
// path: InitState builds a sessionView under stateKey and starts
// the observer goroutine. CloseSession drains and exits.
func TestExtension_InitState_AllocatesAndSpawns(t *testing.T) {
	ext := New(nil)
	state := fixture.NewTestSessionState("ses-test-1")
	if err := ext.InitState(context.Background(), state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	v, ok := state.Value(stateKey)
	if !ok {
		t.Fatal("InitState did not stash view under stateKey")
	}
	if v == nil {
		t.Fatal("stashed view is nil")
	}
	if err := ext.CloseSession(context.Background(), state); err != nil {
		t.Errorf("CloseSession: %v", err)
	}
}

// TestExtension_OnFrameEmit_ForceEmitOnInquiry verifies the
// lifecycle-force-emit path: an InquiryRequest frame triggers
// an immediate emit (skips the debounce window).
func TestExtension_OnFrameEmit_ForceEmitOnInquiry(t *testing.T) {
	ext := New(nil)
	state := fixture.NewTestSessionState("ses-test-2")
	if err := ext.InitState(context.Background(), state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	defer ext.CloseSession(context.Background(), state)

	// Shorten debounce in the per-session view so the test
	// doesn't wait. fromState returns the *sessionView.
	if v := fromState(state); v != nil {
		v.debounce = 50 * time.Millisecond
	}

	req := protocol.NewInquiryRequest("ses-test-2", protocol.ParticipantInfo{},
		protocol.InquiryRequestPayload{
			RequestID: "req-1",
			Type:      protocol.InquiryTypeApproval,
			Question:  "Run?",
		})
	ext.OnFrameEmit(context.Background(), state, req)

	// Force-emit should land synchronously through the
	// goroutine; poll for up to 1s.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if len(state.Emitted()) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	frames := state.Emitted()
	if len(frames) == 0 {
		t.Fatal("no liveview frame emitted within 1s of InquiryRequest")
	}
	ef, ok := frames[0].(*protocol.ExtensionFrame)
	if !ok {
		t.Fatalf("emitted frame type %T, want *ExtensionFrame", frames[0])
	}
	if ef.Payload.Extension != providerName || ef.Payload.Op != opStatus {
		t.Errorf("frame metadata: extension=%q op=%q, want %q/%q",
			ef.Payload.Extension, ef.Payload.Op, providerName, opStatus)
	}
	var body map[string]any
	if err := json.Unmarshal(ef.Payload.Data, &body); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if body["session_id"] != "ses-test-2" {
		t.Errorf("session_id = %v, want ses-test-2", body["session_id"])
	}
	if body["pending_inquiry"] == nil {
		t.Error("pending_inquiry missing from emitted payload")
	}
}

// TestExtension_OnFrameEmit_NoEmitOnTrivialFrame verifies a
// non-lifecycle frame (e.g. a SystemMarker) does NOT trigger an
// immediate emit. With the debounce window still open, the test
// observes no frame within the inner timeout.
func TestExtension_OnFrameEmit_NoEmitOnTrivialFrame(t *testing.T) {
	ext := New(nil)
	state := fixture.NewTestSessionState("ses-test-3")
	if err := ext.InitState(context.Background(), state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	defer ext.CloseSession(context.Background(), state)

	// Long debounce so we don't accidentally see a debounced
	// emit within the test window.
	if v := fromState(state); v != nil {
		v.debounce = 10 * time.Second
	}

	marker := protocol.NewSystemMarker("ses-test-3", protocol.ParticipantInfo{}, "audit", nil)
	ext.OnFrameEmit(context.Background(), state, marker)

	// Wait briefly; expect NO emit (debounce holds, no
	// lifecycle change).
	time.Sleep(100 * time.Millisecond)
	if got := len(state.Emitted()); got != 0 {
		t.Errorf("emitted %d frames for trivial event, want 0", got)
	}
}

// TestExtension_OnChildFrame_FoldsLiveviewStatusFromChild covers
// the recursive aggregation path: a child's own liveview status
// frame, arriving via OnChildFrame, populates the parent's
// children map and triggers a force-emit.
func TestExtension_OnChildFrame_FoldsLiveviewStatusFromChild(t *testing.T) {
	ext := New(nil)
	state := fixture.NewTestSessionState("ses-parent")
	if err := ext.InitState(context.Background(), state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	defer ext.CloseSession(context.Background(), state)

	if v := fromState(state); v != nil {
		v.debounce = 50 * time.Millisecond
	}

	childData, _ := json.Marshal(map[string]any{
		"session_id":      "ses-child",
		"lifecycle_state": "active",
	})
	childFrame := protocol.NewExtensionFrame(
		"ses-child",
		protocol.ParticipantInfo{},
		providerName, protocol.CategoryMarker, opStatus, childData,
	)
	ext.OnChildFrame(context.Background(), state, "ses-child", childFrame)

	// Force-emit fires on child status fold; poll up to 1s.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if len(state.Emitted()) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	frames := state.Emitted()
	if len(frames) == 0 {
		t.Fatal("no liveview frame after child fold")
	}
	ef := frames[0].(*protocol.ExtensionFrame)
	var body map[string]any
	if err := json.Unmarshal(ef.Payload.Data, &body); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	kids, ok := body["children"].(map[string]any)
	if !ok {
		t.Fatalf("children map missing or wrong type: %+v", body["children"])
	}
	if _, ok := kids["ses-child"]; !ok {
		t.Errorf("ses-child entry missing from children map: %+v", kids)
	}
}

// TestExtension_OnChildFrame_SessionTerminatedDropsChildEntry
// verifies that a child's SessionTerminated frame removes the
// child entry from the parent's projection cache. Without this,
// the children map kept growing across subagent lifetimes (the
// initial 5.1b bug observed in production logs).
func TestExtension_OnChildFrame_SessionTerminatedDropsChildEntry(t *testing.T) {
	ext := New(nil)
	state := fixture.NewTestSessionState("ses-parent-drop")
	if err := ext.InitState(context.Background(), state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	defer ext.CloseSession(context.Background(), state)
	if v := fromState(state); v != nil {
		v.debounce = 50 * time.Millisecond
	}

	// Seed the cache with a child entry via a child status frame.
	childData, _ := json.Marshal(map[string]any{"session_id": "ses-doomed"})
	statusFrame := protocol.NewExtensionFrame(
		"ses-doomed", protocol.ParticipantInfo{},
		providerName, protocol.CategoryMarker, opStatus, childData,
	)
	ext.OnChildFrame(context.Background(), state, "ses-doomed", statusFrame)

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(state.Emitted()) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	priorEmits := len(state.Emitted())
	if priorEmits == 0 {
		t.Fatalf("no emit after seeding child entry")
	}

	// Now deliver a SessionTerminated for the same child; expect a
	// fresh emit whose children map no longer contains the id.
	term := protocol.NewSessionTerminated(
		"ses-doomed", protocol.ParticipantInfo{},
		protocol.SessionTerminatedPayload{Reason: protocol.TerminationCompleted},
	)
	ext.OnChildFrame(context.Background(), state, "ses-doomed", term)

	deadline = time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(state.Emitted()) > priorEmits {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	frames := state.Emitted()
	if len(frames) <= priorEmits {
		t.Fatalf("no fresh emit after SessionTerminated child fold (had %d, still %d)",
			priorEmits, len(frames))
	}
	ef := frames[len(frames)-1].(*protocol.ExtensionFrame)
	var body map[string]any
	if err := json.Unmarshal(ef.Payload.Data, &body); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if kids, ok := body["children"].(map[string]any); ok {
		if _, still := kids["ses-doomed"]; still {
			t.Errorf("ses-doomed still in children map after SessionTerminated: %+v", kids)
		}
	}
	// Dogfood follow-up: terminated child should land in
	// recent_children with the reason carried through.
	rc, ok := body["recent_children"].([]any)
	if !ok || len(rc) == 0 {
		t.Fatalf("recent_children missing or empty in payload: %+v", body["recent_children"])
	}
	first, _ := rc[0].(map[string]any)
	if first["session_id"] != "ses-doomed" {
		t.Errorf("recent_children[0].session_id = %v; want ses-doomed", first["session_id"])
	}
	if first["reason"] != protocol.TerminationCompleted {
		t.Errorf("recent_children[0].reason = %v; want %q",
			first["reason"], protocol.TerminationCompleted)
	}
}

// TestExtension_ReportStatus_OwnActivity verifies the
// StatusReporter capability: liveview exposes its own activity
// projection as JSON (lifecycle_state, last_tool_call,
// pending_inquiry, turns_used). Siblings (other extensions'
// liveview) call this when assembling their emit payload.
func TestExtension_ReportStatus_OwnActivity(t *testing.T) {
	ext := New(nil)
	state := fixture.NewTestSessionState("ses-report")
	if err := ext.InitState(context.Background(), state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	defer ext.CloseSession(context.Background(), state)

	// Empty session — ReportStatus returns nil.
	if got := ext.ReportStatus(context.Background(), state); got != nil {
		t.Errorf("empty session ReportStatus = %s, want nil", got)
	}

	// Push a SessionStatus frame; fold sets lifecycle_state.
	statusFrame := protocol.NewSessionStatus("ses-report", protocol.ParticipantInfo{},
		protocol.SessionStatusActive, "user_message")
	ext.OnFrameEmit(context.Background(), state, statusFrame)

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if data := ext.ReportStatus(context.Background(), state); data != nil {
			var body map[string]any
			if err := json.Unmarshal(data, &body); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if body["lifecycle_state"] == protocol.SessionStatusActive {
				return // PASS
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("ReportStatus did not surface lifecycle_state within 500ms")
}

// TestExtension_OutboxChannelOverflow asserts the per-session
// observer channel drops on overflow rather than blocking. Push
// > channelBuffer frames without letting the goroutine drain
// (by closing the session-state's inbox path).
func TestExtension_OutboxChannelOverflow(t *testing.T) {
	ext := New(nil)
	state := fixture.NewTestSessionState("ses-overflow")
	if err := ext.InitState(context.Background(), state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	defer ext.CloseSession(context.Background(), state)

	// Push 2× channelBuffer frames synchronously. None should
	// block; the test passes by simply returning.
	marker := protocol.NewSystemMarker("ses-overflow", protocol.ParticipantInfo{}, "x", nil)
	for i := 0; i < channelBuffer*2; i++ {
		done := make(chan struct{})
		go func() {
			ext.OnFrameEmit(context.Background(), state, marker)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("OnFrameEmit blocked at iteration %d (overflow not detected)", i)
		}
	}
	// Verify the view is intact — type assertion safe.
	if v := fromState(state); v == nil {
		t.Errorf("sessionView gone after overflow")
	}
}

// TestExtension_RecentActivity_RollingWindow — phase 5.1c S1.
// Each *protocol.ToolCall observation prepends to a rolling
// window capped at recentToolWindow (most-recent first); the
// emit payload carries the window under `recent_activity`.
func TestExtension_RecentActivity_RollingWindow(t *testing.T) {
	ext := New(nil)
	state := fixture.NewTestSessionState("ses-recent-1")
	if err := ext.InitState(context.Background(), state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	defer ext.CloseSession(context.Background(), state)
	if v := fromState(state); v != nil {
		v.debounce = 30 * time.Millisecond
	}

	// Submit recentToolWindow+1 ToolCall frames — the oldest
	// should drop out of the window.
	tools := []string{"first", "second", "third", "fourth"}
	for _, name := range tools {
		ext.OnFrameEmit(context.Background(), state,
			protocol.NewToolCall("ses-recent-1",
				protocol.ParticipantInfo{ID: "a", Kind: protocol.ParticipantAgent},
				"tc-"+name, name, nil))
	}
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(state.Emitted()) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	frames := state.Emitted()
	if len(frames) == 0 {
		t.Fatalf("no emit after ToolCall frames")
	}
	// Use the LAST emit (debounce coalesces; force-emit per tool
	// call fires immediately so we expect ≥ 4 emits — pick last).
	ef := frames[len(frames)-1].(*protocol.ExtensionFrame)
	var body map[string]any
	if err := json.Unmarshal(ef.Payload.Data, &body); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	recent, ok := body["recent_activity"].([]any)
	if !ok {
		t.Fatalf("recent_activity missing or wrong type: %+v", body["recent_activity"])
	}
	if len(recent) != recentToolWindow {
		t.Fatalf("recent_activity len = %d; want %d", len(recent), recentToolWindow)
	}
	// Most-recent first.
	first := recent[0].(map[string]any)
	if first["name"] != "fourth" {
		t.Errorf("recent_activity[0].name = %v; want fourth", first["name"])
	}
}

// TestExtension_NewExtensionSatisfiesContract is a compile-time-
// style guard that *Extension implements every documented
// capability. The actual assertion is in extension.go (interface
// asserts); this test runs them at test-time too so a future
// refactor doesn't accidentally widen the capability set.
func TestExtension_NewExtensionSatisfiesContract(t *testing.T) {
	var ext *Extension = New(nil)
	var _ extension.Extension = ext
	var _ extension.StateInitializer = ext
	var _ extension.FrameObserver = ext
	var _ extension.ChildFrameObserver = ext
	var _ extension.StatusReporter = ext
	var _ extension.Closer = ext
}
