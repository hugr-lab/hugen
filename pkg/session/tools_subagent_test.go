package session

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/internal/fixture"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// us1WithSession is the standard test ctx that pretends a tool
// dispatcher has already wired the calling session into the
// dispatch ctx. The session-tool handlers under test receive their
// *Session through the receiver, but extension-aware paths read it
// via extension.SessionStateFromContext, so attach the state under
// that key.
func us1WithSession(parent *Session) context.Context {
	return extension.WithSessionState(context.Background(), parent)
}

// ---------- spawn_mission ----------

// TestCallSpawnMission_Happy verifies the singular spawn variant
// returns a spawnMissionResult envelope with a registered child
// in parent.children. wait="sync" forces the synchronous shape so
// the test doesn't depend on the root-defaults-to-async rule.
func TestCallSpawnMission_Happy(t *testing.T) {
	parent, cleanup := newTestParent(t, withMissionDispatcher("analyst"))
	defer cleanup()

	out, err := parent.callSpawnMission(us1WithSession(parent),
		json.RawMessage(`{"name":"m","goal":"analyse northwind","skill":"analyst","wait":"sync"}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var got spawnMissionResult
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal singular result: %v\noutput=%s", err, out)
	}
	if got.SessionID == "" || got.Depth != 1 {
		t.Errorf("unexpected result %+v", got)
	}
	parent.childMu.Lock()
	_, inChildren := parent.children[got.SessionID]
	parent.childMu.Unlock()
	if !inChildren {
		t.Errorf("spawned mission %q not in parent.children", got.SessionID)
	}
}

// TestCallSpawnMission_RootDefaultsToAsync covers the 5.4.c.3
// rule: when the caller is a chat root (depth 0) and `wait` is
// omitted, the runtime fills `async` so the auto-summary turn
// fires when the mission completes. Weak models that drop the
// optional field still get the documented "_root" behaviour. The
// returned envelope is the async `spawnMissionResult` shape with
// status "running" — distinct from sync's spawnSubagentResult
// shape.
func TestCallSpawnMission_RootDefaultsToAsync(t *testing.T) {
	parent, cleanup := newTestParent(t, withMissionDispatcher("analyst"))
	defer cleanup()
	if parent.depth != 0 {
		t.Fatalf("test parent must be a root session; got depth=%d", parent.depth)
	}

	out, err := parent.callSpawnMission(us1WithSession(parent),
		json.RawMessage(`{"name":"m","goal":"analyse northwind","skill":"analyst"}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var got spawnMissionResult
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal async result: %v\noutput=%s", err, out)
	}
	if got.Status != "running" {
		t.Errorf("status = %q, want \"running\" (root → async default): %s", got.Status, out)
	}
	parent.childMu.Lock()
	child := parent.children[got.SessionID]
	parent.childMu.Unlock()
	if child == nil {
		t.Fatalf("child not registered: %s", out)
	}
	if child.asyncSpawnMode != protocol.SubagentRenderAsyncNotify {
		t.Errorf("child.asyncSpawnMode = %v, want SubagentRenderAsyncNotify (root → async default)",
			child.asyncSpawnMode)
	}
}

// TestCallSpawnMission_NoMissionSkill_NoArg_NoDefault verifies a
// missing skill argument with no operator default surfaces a
// structured no_mission_skill envelope and does not spawn anything.
// Phase 4.2.2 §6.
func TestCallSpawnMission_NoMissionSkill_NoArg_NoDefault(t *testing.T) {
	parent, cleanup := newTestParent(t, withMissionDispatcher("analyst"))
	defer cleanup()

	out, err := parent.callSpawnMission(us1WithSession(parent),
		json.RawMessage(`{"name":"m","goal":"analyse northwind"}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	mgr_assertErrorCode(t, out, "no_mission_skill")
	parent.childMu.Lock()
	defer parent.childMu.Unlock()
	if len(parent.children) != 0 {
		t.Errorf("parent.children = %d after no_mission_skill, want 0", len(parent.children))
	}
}

// TestCallSpawnMission_DispatchByDefault verifies an empty skill
// argument falls back to deps.DefaultMissionSkill when the
// dispatcher catalogue has that name registered. Phase 4.2.2 §6.
func TestCallSpawnMission_DispatchByDefault(t *testing.T) {
	parent, cleanup := newTestParent(t, withMissionDispatcher("analyst"))
	defer cleanup()
	parent.deps.DefaultMissionSkill = "analyst"

	out, err := parent.callSpawnMission(us1WithSession(parent),
		json.RawMessage(`{"name":"m","goal":"analyse northwind","wait":"sync"}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var got spawnMissionResult
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v\noutput=%s", err, out)
	}
	if got.SessionID == "" {
		t.Errorf("default-dispatch spawn empty: %s", out)
	}
}

// TestCallSpawnMission_RejectsNonDispatcherSkill verifies an
// explicit skill argument naming a non-registered mission
// dispatcher is rejected with no_mission_skill — root cannot
// invent skill names. Phase 4.2.2 §6.
func TestCallSpawnMission_RejectsNonDispatcherSkill(t *testing.T) {
	parent, cleanup := newTestParent(t, withMissionDispatcher("analyst"))
	defer cleanup()

	out, err := parent.callSpawnMission(us1WithSession(parent),
		json.RawMessage(`{"name":"m","goal":"x","skill":"non-existent-skill"}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	mgr_assertErrorCode(t, out, "no_mission_skill")
}

// TestCallSpawnMission_GoalRequired verifies the goal field is
// validated before any spawn happens.
func TestCallSpawnMission_GoalRequired(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	out, _ := parent.callSpawnMission(us1WithSession(parent),
		json.RawMessage(`{"name":"m","goal":""}`))
	mgr_assertErrorCode(t, out, "bad_request")

	out, _ = parent.callSpawnMission(us1WithSession(parent),
		json.RawMessage(`{}`))
	mgr_assertErrorCode(t, out, "bad_request")

	parent.childMu.Lock()
	defer parent.childMu.Unlock()
	if len(parent.children) != 0 {
		t.Errorf("parent.children = %d after validation failure, want 0", len(parent.children))
	}
}

// Mission-PDCA (design 003): the on_mission_start hook
// (plan.SystemSet + whiteboard.SystemInit + FirstMessageOverride)
// has been removed. Missions are driven by mission ext's
// MissionAutoRunner — no plan/whiteboard pre-writes, no
// UserMessage-based supervisor. The replacement coverage lives
// in pkg/extension/mission's auto_runner tests +
// pkg/session/integration_mission_pdca_test.go (end-to-end via
// real Executor).

// ---------- spawn_wave ----------

// TestCallSpawnWave_BadRequest covers the empty-subagents and
// invalid-JSON refusals before any spawn happens.
// ---------- spawn_wave / spawn_subagent tests removed by Phase H ----------
// The legacy session:spawn_wave / session:spawn_subagent /
// session:parent_context / session:notify_subagent LLM tools were
// removed under mission-PDCA Phase H (design 003). Runtime owns
// mission dispatch via the mission ext; root delegates only via
// session:spawn_mission. Tests for the deleted handlers are gone
// with them; spawn coverage now lives under spawn_mission +
// pkg/extension/mission/* tests.

// All TestCallSpawnSubagent_* tests removed under mission-PDCA
// Phase H — the legacy LLM tool is gone. Spawn intent / depth /
// session-gone coverage now lives under spawn_mission tests below
// + pkg/extension/mission integration tests.

// ---------- wait_subagents ----------

// TestCallWaitSubagents_Happy_LiveResult drives wait_subagents
// through a synthetic SubagentResult Submit. The test runs the call
// in a goroutine and feeds the result Frame into the parent's inbox
// via Submit; the routing layer hands it to activeToolFeed. The
// Submit here simulates what the parent's pump does in production
// after observing a child's terminal AgentMessage / SessionTerminated
// (phase 4.1c) — wait_subagents is the consumer either way.
func TestCallWaitSubagents_Happy_LiveResult(t *testing.T) {
	parent, cleanup := newTestParent(t, withTestRunLoop())
	defer cleanup()

	// Spawn a real child so the id exists; we'll synthesise a result
	// for it without waiting for real natural termination. Direct
	// parent.Spawn (Go API) — the legacy spawn_subagent LLM tool is
	// gone under mission-PDCA Phase H.
	child, err := parent.Spawn(context.Background(), SpawnSpec{Name: "w", Task: "t"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	childID := child.ID()
	drainOutboxOnce(parent.Outbox()) // subagent_started

	// wait runs in goroutine; its activeToolFeed registration races
	// with the Submit below, so the Run loop's routeInbound finds the
	// feed registered when the synthetic Frame arrives.
	type res struct {
		out []byte
		err error
	}
	done := make(chan res, 1)
	args, _ := json.Marshal(waitSubagentsInput{IDs: []string{childID}})
	go func() {
		out, err := parent.callWaitSubagents(us1WithSession(parent), args)
		done <- res{out: out, err: err}
	}()

	// Give wait_subagents time to register the feed.
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for parent.ActiveToolFeed() == nil {
		select {
		case <-time.After(20 * time.Millisecond):
		case <-deadline.C:
			t.Fatal("activeToolFeed never registered")
		}
	}

	// Now submit the synthetic result. It rides through routeInbound
	// → RouteToolFeed → feed.Feed → wait_subagents.
	result := protocol.NewSubagentResult(parent.ID(), childID, parent.agent.Participant(),
		protocol.SubagentResultPayload{
			SessionID: childID,
			Result:    "done",
			Reason:    protocol.TerminationCompleted,
			TurnsUsed: 3,
		})
	parent.Submit(context.Background(), result)

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("wait err: %v", r.err)
		}
		var rows []waitResultRow
		if err := json.Unmarshal(r.out, &rows); err != nil {
			t.Fatalf("unmarshal wait result: %v\nout=%s", err, r.out)
		}
		if len(rows) != 1 || rows[0].SessionID != childID {
			t.Errorf("rows = %+v, want one row for %q", rows, childID)
		}
		if rows[0].Status != "completed" || rows[0].Result != "done" {
			t.Errorf("row = %+v, want status=completed result=done", rows[0])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("wait_subagents did not return within 3s")
	}
}

// TestCallSpawnMission_Async returns immediately with running
// shape and tags the child for the async-completed render mode.
// Verifies the cap + the legacy spawn_subagent fields still
// surface (mission_id + session_id are aliases).
func TestCallSpawnMission_Async(t *testing.T) {
	parent, cleanup := newTestParent(t, withMissionDispatcher("analyst"))
	defer cleanup()

	out, err := parent.callSpawnMission(us1WithSession(parent),
		json.RawMessage(`{"name":"m","goal":"explore","skill":"analyst","wait":"async"}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var got spawnMissionResult
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v out=%s", err, out)
	}
	if got.Status != "running" {
		t.Errorf("status = %q, want running: %s", got.Status, out)
	}
	if got.MissionID == "" || got.SessionID == "" || got.MissionID != got.SessionID {
		t.Errorf("mission_id / session_id wiring: %+v", got)
	}
	// Verify the async-notify tag landed on the child so the
	// pump's projection produces the async template at completion.
	parent.childMu.Lock()
	child := parent.children[got.SessionID]
	parent.childMu.Unlock()
	if child == nil {
		t.Fatalf("spawned child not in parent.children")
	}
	if child.asyncSpawnMode != protocol.SubagentRenderAsyncNotify {
		t.Errorf("asyncSpawnMode = %q, want %q",
			child.asyncSpawnMode, protocol.SubagentRenderAsyncNotify)
	}
}

// TestCallSpawnMission_AsyncSilent suppresses the history
// projection of the terminal subagent_result while still
// persisting the event.
func TestCallSpawnMission_AsyncSilent(t *testing.T) {
	parent, cleanup := newTestParent(t, withMissionDispatcher("analyst"))
	defer cleanup()

	out, err := parent.callSpawnMission(us1WithSession(parent),
		json.RawMessage(`{"name":"m","goal":"explore","skill":"analyst","wait":"async","on_complete":"silent"}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var got spawnMissionResult
	_ = json.Unmarshal(out, &got)
	parent.childMu.Lock()
	child := parent.children[got.SessionID]
	parent.childMu.Unlock()
	if child == nil || child.asyncSpawnMode != protocol.SubagentRenderSilent {
		t.Errorf("asyncSpawnMode = %q, want %q",
			child.asyncSpawnMode, protocol.SubagentRenderSilent)
	}
}

// TestCallSpawnMission_AsyncCap exercises the § 4.5 per-root
// concurrency check. Sets the deps cap to a small value, fills
// parent.children up to it, then expects the next async spawn
// to surface "too_many_async".
func TestCallSpawnMission_AsyncCap(t *testing.T) {
	parent, cleanup := newTestParent(t, withMissionDispatcher("analyst"))
	defer cleanup()
	parent.deps.MaxAsyncMissionsPerRoot = 1

	// First async spawn — fills the cap.
	if _, err := parent.callSpawnMission(us1WithSession(parent),
		json.RawMessage(`{"name":"m","goal":"a","skill":"analyst","wait":"async"}`)); err != nil {
		t.Fatalf("first async: %v", err)
	}
	// Second async spawn — rejected.
	out, err := parent.callSpawnMission(us1WithSession(parent),
		json.RawMessage(`{"name":"m","goal":"b","skill":"analyst","wait":"async"}`))
	if err != nil {
		t.Fatalf("second async call: %v", err)
	}
	mgr_assertErrorCode(t, out, "too_many_async")
}

// TestCallSpawnMission_TwoAsyncInOneTurn — phase 5.1c.async-root:
// root can spawn two independent missions back-to-back in the
// same turn (bucket B × 2). Both children must land in
// parent.children with the async-notify tag.
func TestCallSpawnMission_TwoAsyncInOneTurn(t *testing.T) {
	parent, cleanup := newTestParent(t, withMissionDispatcher("analyst"))
	defer cleanup()
	// Generous cap; the bucket-B duplication should fit comfortably.
	parent.deps.MaxAsyncMissionsPerRoot = 4

	out1, err := parent.callSpawnMission(us1WithSession(parent),
		json.RawMessage(`{"name":"m","goal":"orders summary","skill":"analyst","wait":"async"}`))
	if err != nil {
		t.Fatalf("first async: %v", err)
	}
	out2, err := parent.callSpawnMission(us1WithSession(parent),
		json.RawMessage(`{"name":"m","goal":"inventory count","skill":"analyst","wait":"async"}`))
	if err != nil {
		t.Fatalf("second async: %v", err)
	}
	var got1, got2 spawnMissionResult
	if err := json.Unmarshal(out1, &got1); err != nil {
		t.Fatalf("unmarshal out1: %v", err)
	}
	if err := json.Unmarshal(out2, &got2); err != nil {
		t.Fatalf("unmarshal out2: %v", err)
	}
	if got1.Status != "running" || got2.Status != "running" {
		t.Errorf("both statuses must be running: %q / %q", got1.Status, got2.Status)
	}
	if got1.SessionID == got2.SessionID || got1.SessionID == "" || got2.SessionID == "" {
		t.Errorf("session ids must be distinct + non-empty: %q / %q",
			got1.SessionID, got2.SessionID)
	}
	parent.childMu.Lock()
	defer parent.childMu.Unlock()
	if len(parent.children) != 2 {
		t.Errorf("len(children) = %d, want 2", len(parent.children))
	}
	for _, sid := range []string{got1.SessionID, got2.SessionID} {
		ch, ok := parent.children[sid]
		if !ok {
			t.Errorf("child %q missing from parent.children", sid)
			continue
		}
		if ch.asyncSpawnMode != protocol.SubagentRenderAsyncNotify {
			t.Errorf("child %q asyncSpawnMode = %q, want %q",
				sid, ch.asyncSpawnMode, protocol.SubagentRenderAsyncNotify)
		}
	}
}

// TestCallSpawnMission_BadWait rejects an unknown wait value.
func TestCallSpawnMission_BadWait(t *testing.T) {
	parent, cleanup := newTestParent(t, withMissionDispatcher("analyst"))
	defer cleanup()
	out, _ := parent.callSpawnMission(us1WithSession(parent),
		json.RawMessage(`{"name":"m","goal":"x","skill":"analyst","wait":"sometime"}`))
	mgr_assertErrorCode(t, out, "bad_request")
}

// TestCallSpawnMission_TimeoutRequiresMs rejects wait=timeout
// without a positive timeout_ms.
func TestCallSpawnMission_TimeoutRequiresMs(t *testing.T) {
	parent, cleanup := newTestParent(t, withMissionDispatcher("analyst"))
	defer cleanup()
	out, _ := parent.callSpawnMission(us1WithSession(parent),
		json.RawMessage(`{"name":"m","goal":"x","skill":"analyst","wait":"timeout"}`))
	mgr_assertErrorCode(t, out, "bad_request")
}

// TestCallNotifySubagent_* removed under mission-PDCA Phase H —
// the legacy notify_subagent LLM tool is gone. Mid-mission notify
// is now exposed at root via mission:notify; the Go API
// (Session.NotifyChild) stays for runtime-level interrupt delivery.

// TestCallInquire_Approval_HappyPath drives the full session:
// inquire round-trip at root: emit request, deliver response via
// the internal dispatcher, observe the approval payload.
func TestCallInquire_Approval_HappyPath(t *testing.T) {
	parent, cleanup := newTestParent(t, withTestRunLoop())
	defer cleanup()

	type res struct {
		out []byte
		err error
	}
	done := make(chan res, 1)
	args, _ := json.Marshal(inquireInput{
		Type:      protocol.InquiryTypeApproval,
		Question:  "Run `rm -rf /tmp/foo`?",
		TimeoutMs: 2000,
	})
	go func() {
		out, err := parent.callInquire(us1WithSession(parent), args)
		done <- res{out: out, err: err}
	}()

	// Wait for the feed to register so we know the request was
	// emitted and pending registered.
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for parent.ActiveToolFeed() == nil {
		select {
		case <-time.After(10 * time.Millisecond):
		case <-deadline.C:
			t.Fatal("inquire activeToolFeed never registered")
		}
	}
	// Extract the RequestID from the emitted request frame —
	// drainOutbox returns the InquiryRequest the tool body
	// pushed to outbox via emit.
	var requestID string
	for i := 0; i < 5 && requestID == ""; i++ {
		select {
		case f := <-parent.Outbox():
			if req, ok := f.(*protocol.InquiryRequest); ok {
				requestID = req.Payload.RequestID
			}
		case <-time.After(200 * time.Millisecond):
		}
	}
	if requestID == "" {
		t.Fatal("InquiryRequest not observed on parent outbox")
	}

	// Synthesise the adapter's response.
	approved := true
	resp := protocol.NewInquiryResponse(parent.ID(), parent.agent.Participant(),
		protocol.InquiryResponsePayload{
			RequestID:       requestID,
			CallerSessionID: parent.ID(),
			Approved:        &approved,
			Reason:          "ok",
		})
	parent.Submit(context.Background(), resp)

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("inquire err: %v out=%s", r.err, r.out)
		}
		var got approvalResult
		if err := json.Unmarshal(r.out, &got); err != nil {
			t.Fatalf("unmarshal: %v out=%s", err, r.out)
		}
		if !got.Approved || got.Reason != "ok" {
			t.Errorf("unexpected result %+v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("inquire did not return within 3s")
	}
}

// TestCallInquire_Timeout fires the per-call timer and surfaces
// the default-deny envelope.
func TestCallInquire_Timeout(t *testing.T) {
	parent, cleanup := newTestParent(t, withTestRunLoop())
	defer cleanup()

	args, _ := json.Marshal(inquireInput{
		Type:      protocol.InquiryTypeApproval,
		Question:  "anything?",
		TimeoutMs: 100,
	})
	out, err := parent.callInquire(us1WithSession(parent), args)
	if err != nil {
		t.Fatalf("inquire err: %v", err)
	}
	var got approvalResult
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v out=%s", err, out)
	}
	if !got.Timeout || got.Approved || got.DefaultAction != "deny" {
		t.Errorf("expected timeout=true approved=false default=deny; got %+v", got)
	}
}

// TestCallInquire_BadType rejects unknown inquiry types.
func TestCallInquire_BadType(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()
	args, _ := json.Marshal(map[string]any{"type": "weird", "question": "x"})
	out, _ := parent.callInquire(us1WithSession(parent), args)
	mgr_assertErrorCode(t, out, "bad_request")
}

// TestCallInquire_MissingTypeDefaultsToClarification — regression
// for the dogfood failure where Gemma-class models omit the
// schema-required `type` field. We default to clarification +
// warn-log so the inquiry pipeline runs instead of returning
// bad_request and triggering retry storms.
func TestCallInquire_MissingTypeDefaultsToClarification(t *testing.T) {
	parent, cleanup := newTestParent(t, withTestRunLoop())
	defer cleanup()
	args, _ := json.Marshal(map[string]any{
		"question":  "Which dataset?",
		"options":   []string{"a", "b"},
		"timeout_ms": 100, // short so the test doesn't hang on the wait
	})
	out, err := parent.callInquire(us1WithSession(parent), args)
	if err != nil {
		t.Fatalf("callInquire returned err: %v", err)
	}
	// With type defaulted to clarification, the call should run
	// to its (short) timeout — NOT return a bad_request envelope.
	var got clarificationResult
	if uerr := json.Unmarshal(out, &got); uerr != nil {
		t.Fatalf("unmarshal: %v out=%s", uerr, out)
	}
	if !got.Timeout {
		t.Errorf("expected timeout envelope after default-to-clarification; got %s", out)
	}
}

// TestIsParentNote covers the FromSession predicate the wait
// feed uses to discriminate parent notes from unrelated system
// messages. Root sessions (no parent) never match.
func TestIsParentNote(t *testing.T) {
	root := &Session{id: "root-1"}
	child := &Session{id: "child-1", parent: root}
	author := protocol.ParticipantInfo{ID: "test", Kind: protocol.ParticipantSystem}

	parentMsg := protocol.NewSystemMessage(child.id, author, "parent_note", "do the thing")
	parentMsg.BaseFrame.FromSession = root.id

	otherMsg := protocol.NewSystemMessage(child.id, author, "stuck_nudge", "loop")
	user := protocol.NewUserMessage(child.id, author, "hi")

	if !isParentNote(parentMsg, child) {
		t.Errorf("parent-authored SystemMessage to child should be a parent note")
	}
	if isParentNote(otherMsg, child) {
		t.Errorf("SystemMessage from same session is not a parent note")
	}
	if isParentNote(user, child) {
		t.Errorf("UserMessage should never be classified as a parent note")
	}
	if isParentNote(parentMsg, root) {
		t.Errorf("root sessions (no parent) should never see a parent note")
	}
}

// TestCallWaitSubagents_UserFollowUp_Interrupts verifies γ: a
// UserMessage delivered to a root parent while wait_subagents is
// blocked short-circuits the wait with a rendered reframe instead
// of continuing to wait for the child. The rendered text quotes
// the user's input and lists the still-pending subagent.
func TestCallWaitSubagents_UserFollowUp_Interrupts(t *testing.T) {
	parent, cleanup := newTestParent(t, withTestRunLoop())
	defer cleanup()

	// Spawn a real child so the id exists; the child won't terminate
	// during this test — the interrupt fires from a different path.
	// Direct parent.Spawn (Go API) — mission-PDCA Phase H removed the
	// legacy spawn_subagent LLM tool.
	child, err := parent.Spawn(context.Background(), SpawnSpec{
		Name: "w", Task: "explore catalog", Role: "explorer",
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	childID := child.ID()
	drainOutboxOnce(parent.Outbox()) // subagent_started

	type res struct {
		out []byte
		err error
	}
	done := make(chan res, 1)
	args, _ := json.Marshal(waitSubagentsInput{IDs: []string{childID}})
	go func() {
		out, err := parent.callWaitSubagents(us1WithSession(parent), args)
		done <- res{out: out, err: err}
	}()

	// Wait for the feed to register.
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for parent.ActiveToolFeed() == nil {
		select {
		case <-time.After(20 * time.Millisecond):
		case <-deadline.C:
			t.Fatal("activeToolFeed never registered")
		}
	}

	// Deliver the user follow-up.
	user := protocol.NewUserMessage(parent.ID(), parent.agent.Participant(),
		"actually pivot to logs")
	parent.Submit(context.Background(), user)

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("wait err: %v", r.err)
		}
		var result waitInterruptResult
		if err := json.Unmarshal(r.out, &result); err != nil {
			t.Fatalf("unmarshal interrupt: %v\nout=%s", err, r.out)
		}
		if !result.Interrupted {
			t.Fatalf("expected interrupted=true; got %+v", result)
		}
		if result.Reason != "user_follow_up" {
			t.Errorf("reason = %q, want user_follow_up", result.Reason)
		}
		if !strings.Contains(result.Instructions, "actually pivot to logs") {
			t.Errorf("instructions missing user text: %q", result.Instructions)
		}
		if !strings.Contains(result.Instructions, childID) {
			t.Errorf("instructions missing pending child id %q: %q",
				childID, result.Instructions)
		}
		if len(result.Pending) != 1 || result.Pending[0].ID != childID {
			t.Errorf("pending = %+v, want one row for %q", result.Pending, childID)
		}
		if result.Pending[0].Role != "explorer" {
			t.Errorf("pending row role = %q, want explorer", result.Pending[0].Role)
		}
		if result.Pending[0].Goal != "explore catalog" {
			t.Errorf("pending row goal = %q, want 'explore catalog'", result.Pending[0].Goal)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("wait_subagents did not return interrupt within 3s")
	}
}

// TestCallWaitSubagents_CachedShortCircuit pre-seeds parent's events
// with a SubagentResult, then calls wait_subagents — it must return
// immediately from drainCachedSubagentResults without ever
// registering an activeToolFeed.
func TestCallWaitSubagents_CachedShortCircuit(t *testing.T) {
	testStore := fixture.NewTestStore()
	parent, cleanup := newTestParent(t, withTestStore(testStore))
	defer cleanup()

	cached := protocol.NewSubagentResult(parent.ID(), "child-cached", parent.agent.Participant(),
		protocol.SubagentResultPayload{
			SessionID: "child-cached",
			Result:    "from-store",
			Reason:    protocol.TerminationCompleted,
		})
	row, summary, _ := FrameToEventRow(cached, parent.agent.ID())
	if err := testStore.AppendEvent(context.Background(), row, summary); err != nil {
		t.Fatalf("seed: %v", err)
	}

	args, _ := json.Marshal(waitSubagentsInput{IDs: []string{"child-cached"}})
	out, err := parent.callWaitSubagents(us1WithSession(parent), args)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var rows []waitResultRow
	if err := json.Unmarshal(out, &rows); err != nil {
		t.Fatalf("unmarshal: %v\nout=%s", err, out)
	}
	if len(rows) != 1 || rows[0].Result != "from-store" {
		t.Errorf("rows = %+v, want one cached row", rows)
	}
	if parent.ActiveToolFeed() != nil {
		t.Error("activeToolFeed left registered after fully-cached drain")
	}
}

// TestCallWaitSubagents_EmptyIDsWithNoChildren returns an empty
// result array immediately — phase 5.1b: ids is optional, and a
// session with no children has nothing to wait for. Previously
// this path returned bad_request; the new contract is "wait for
// every direct child" semantics with the natural empty case
// short-circuiting.
func TestCallWaitSubagents_EmptyIDsWithNoChildren(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	out, err := parent.callWaitSubagents(us1WithSession(parent),
		json.RawMessage(`{"ids":[]}`))
	if err != nil {
		t.Fatalf("callWaitSubagents: %v", err)
	}
	if string(out) != "[]" {
		t.Errorf("empty ids with no children: got %s, want []", out)
	}

	// Also accept an absent ids field entirely.
	out, err = parent.callWaitSubagents(us1WithSession(parent),
		json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("callWaitSubagents absent ids: %v", err)
	}
	if string(out) != "[]" {
		t.Errorf("absent ids with no children: got %s, want []", out)
	}
}

// TestCallWaitSubagents_RejectsUnknownID covers the
// hallucinated-id guard: an explicit id that isn't a current
// direct child (and isn't cached as a terminated result) returns
// not_a_child with the real children list. Without this, Gemma's
// `mission_id_from_step1` placeholder would block the tool feed
// until the step budget expires.
func TestCallWaitSubagents_RejectsUnknownID(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	// No children spawned → any id is bogus.
	out, _ := parent.callWaitSubagents(us1WithSession(parent),
		json.RawMessage(`{"ids":["mission_id_from_step1"]}`))
	mgr_assertErrorCode(t, out, "not_a_child")
}

// ---------- subagent_runs ----------

// TestCallSubagentRuns_Happy seeds a child's events and verifies
// pagination + next_since_seq cursor.
func TestCallSubagentRuns_Happy(t *testing.T) {
	testStore := fixture.NewTestStore()
	parent, cleanup := newTestParent(t, withTestStore(testStore))
	defer cleanup()

	child, err := parent.Spawn(context.Background(), SpawnSpec{Task: "t"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	// Seed five additional events on the child.
	for i := 0; i < 5; i++ {
		_ = testStore.AppendEvent(context.Background(), EventRow{
			ID:        "ev-x",
			SessionID: child.ID(),
			AgentID:   "a1",
			EventType: string(protocol.KindAgentMessage),
			Author:    "a1",
			Content:   "msg",
			CreatedAt: time.Now(),
		}, "")
	}

	args, _ := json.Marshal(subagentRunsInput{
		SessionID: child.ID(),
		Limit:     3,
	})
	out, err := parent.callSubagentRuns(us1WithSession(parent), args)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var got subagentRunsOutput
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Events) != 3 {
		t.Errorf("len events = %d, want 3 (limit honoured)", len(got.Events))
	}
	if got.NextSinceSeq <= 0 {
		t.Errorf("next_since_seq = %d, want > 0 (more events remain)", got.NextSinceSeq)
	}
}

// TestCallSubagentRuns_NotAChild verifies the cross-session read
// gate: a session belonging to a different parent surfaces
// not_a_child even when it exists.
func TestCallSubagentRuns_NotAChild(t *testing.T) {
	testStore := fixture.NewTestStore()
	parent, cleanup := newTestParent(t, withTestStore(testStore))
	defer cleanup()

	// Create a row that's not a child of parent.
	other := SessionRow{ID: "ses-other", AgentID: "a1", Status: StatusActive,
		ParentSessionID: "ses-different-parent"}
	_ = testStore.OpenSession(context.Background(), other)

	args, _ := json.Marshal(subagentRunsInput{SessionID: "ses-other"})
	out, _ := parent.callSubagentRuns(us1WithSession(parent), args)
	mgr_assertErrorCode(t, out, "not_a_child")

	// Unknown session also rejected.
	args, _ = json.Marshal(subagentRunsInput{SessionID: "ses-unknown"})
	out, _ = parent.callSubagentRuns(us1WithSession(parent), args)
	mgr_assertErrorCode(t, out, "session_not_found")
}

// TestCallSubagentRuns_HardCap clamps the requested limit to
// subagentRunsHardCap (500). The test just verifies the clamp by
// sending limit > cap and asserting the call doesn't panic / err
// — pagination correctness is covered above.
func TestCallSubagentRuns_HardCap(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	child, err := parent.Spawn(context.Background(), SpawnSpec{Task: "t"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	args, _ := json.Marshal(subagentRunsInput{
		SessionID: child.ID(),
		Limit:     5000, // request beyond cap
	})
	out, err := parent.callSubagentRuns(us1WithSession(parent), args)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if strings.Contains(string(out), `"error"`) {
		t.Errorf("hard-cap clamp produced error: %s", out)
	}
}

// ---------- subagent_cancel ----------

// TestCallSubagentCancel_Happy spawns a child, cancels it, and
// asserts the child's goroutine exits with the expected reason.
func TestCallSubagentCancel_Happy(t *testing.T) {
	testStore := fixture.NewTestStore()
	parent, cleanup := newTestParent(t, withTestStore(testStore))
	defer cleanup()

	child, err := parent.Spawn(context.Background(), SpawnSpec{Task: "t"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	drainOutboxOnce(parent.Outbox()) // subagent_started

	args, _ := json.Marshal(subagentCancelInput{
		SessionID: child.ID(),
		Reason:    "user wants out",
	})
	out, err := parent.callSubagentCancel(us1WithSession(parent), args)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var got subagentCancelOutput
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.OK {
		t.Errorf("got.OK = false, want true")
	}

	select {
	case <-child.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("child did not exit within 2s")
	}
	events, _ := testStore.ListEvents(context.Background(), child.ID(), ListEventsOpts{})
	wanted := protocol.TerminationSubagentCancelPrefix + "user wants out"
	if !containsKindWithReason(events, protocol.KindSessionTerminated, wanted) {
		t.Errorf("child terminated with wrong reason; events=%v", kindsWithReasons(events))
	}
}

// TestCallSubagentCancel_NotAChild blocks the cross-tree cancel
// path by ensuring not_a_child surfaces when the target isn't in
// the caller's children. Both sessions share a store so the
// handler's store lookup finds the target row (otherwise it would
// surface session_not_found before the cross-tree check fires).
func TestCallSubagentCancel_NotAChild(t *testing.T) {
	testStore := fixture.NewTestStore()
	parent, cleanup := newTestParent(t, withTestStore(testStore))
	defer cleanup()
	other, otherCleanup := newTestParent(t, withTestStore(testStore))
	defer otherCleanup()

	args, _ := json.Marshal(subagentCancelInput{SessionID: other.ID()})
	out, _ := parent.callSubagentCancel(us1WithSession(parent), args)
	mgr_assertErrorCode(t, out, "not_a_child")
}

// TestCallSubagentCancel_Idempotent calls cancel twice; the second
// should still return ok=true even though the child has already
// exited.
func TestCallSubagentCancel_Idempotent(t *testing.T) {
	testStore := fixture.NewTestStore()
	parent, cleanup := newTestParent(t, withTestStore(testStore))
	defer cleanup()

	child, err := parent.Spawn(context.Background(), SpawnSpec{Task: "t"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	args, _ := json.Marshal(subagentCancelInput{
		SessionID: child.ID(),
		Reason:    "first",
	})
	if _, err := parent.callSubagentCancel(us1WithSession(parent), args); err != nil {
		t.Fatalf("first cancel: %v", err)
	}
	<-child.Done()

	out, err := parent.callSubagentCancel(us1WithSession(parent), args)
	if err != nil {
		t.Fatalf("second cancel: %v", err)
	}
	var got subagentCancelOutput
	_ = json.Unmarshal(out, &got)
	if !got.OK {
		t.Errorf("idempotent second cancel returned ok=false; out=%s", out)
	}
}

// TestCallParentContext_* removed under mission-PDCA Phase H —
// the parent_context LLM tool is gone. Workers now receive a
// structured [Plan context] / [Resolved depends_on] / [Available
// handoffs] surface in their first message instead of pulling
// from the parent's event log at runtime.

// ---------- helpers ----------

// mgr_assertErrorCode unmarshals out as a toolErrorResponse and fails
// the test when err.code does not equal want.
func mgr_assertErrorCode(t *testing.T, out json.RawMessage, want string) {
	t.Helper()
	var resp toolErrorResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("unmarshal tool error: %v\nout=%s", err, out)
	}
	if resp.Error.Code != want {
		t.Errorf("error code = %q, want %q (msg=%q)", resp.Error.Code, want, resp.Error.Message)
	}
}
