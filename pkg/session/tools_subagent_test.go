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
// adapts a goal/inputs payload into a one-entry batch under the
// hood and returns a single object (not an array) shaped like
// spawnSubagentResult.
func TestCallSpawnMission_Happy(t *testing.T) {
	parent, cleanup := newTestParent(t, withMissionDispatcher("analyst"))
	defer cleanup()

	out, err := parent.callSpawnMission(us1WithSession(parent),
		json.RawMessage(`{"goal":"analyse northwind","skill":"analyst"}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var got spawnSubagentResult
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

// TestCallSpawnMission_NoMissionSkill_NoArg_NoDefault verifies a
// missing skill argument with no operator default surfaces a
// structured no_mission_skill envelope and does not spawn anything.
// Phase 4.2.2 §6.
func TestCallSpawnMission_NoMissionSkill_NoArg_NoDefault(t *testing.T) {
	parent, cleanup := newTestParent(t, withMissionDispatcher("analyst"))
	defer cleanup()

	out, err := parent.callSpawnMission(us1WithSession(parent),
		json.RawMessage(`{"goal":"analyse northwind"}`))
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
		json.RawMessage(`{"goal":"analyse northwind"}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var got spawnSubagentResult
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
		json.RawMessage(`{"goal":"x","skill":"non-existent-skill"}`))
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
		json.RawMessage(`{"goal":""}`))
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

// TestCallSpawnMission_OnStartHook_AppliesScaffolding verifies the
// γ on_mission_start hook fires plan.SystemSet + whiteboard.SystemInit
// between Spawn and Submit, AND that a FirstMessageOverride in the
// resolved block replaces the bare goal as the child's first user
// message. Phase 4.2.2 §7.
func TestCallSpawnMission_OnStartHook_AppliesScaffolding(t *testing.T) {
	block := &extension.MissionStartBlock{
		PlanText:             "# Analyse northwind\n1. Explore\n2. Synthesize",
		PlanCurrentStep:      "Explore",
		WhiteboardInit:       true,
		FirstMessageOverride: "User goal: analyse northwind. Start with wave 1.",
	}
	planStub := &stubPlanWriter{}
	wbStub := &stubWhiteboardWriter{}
	parent, cleanup := newTestParent(t,
		withMissionDispatcher("analyst"),
		withMissionStartLookup("analyst", block),
		withPlanSystemWriter(planStub),
		withWhiteboardSystemWriter(wbStub),
	)
	defer cleanup()

	out, err := parent.callSpawnMission(us1WithSession(parent),
		json.RawMessage(`{"goal":"analyse northwind","skill":"analyst"}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var got spawnSubagentResult
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v\noutput=%s", err, out)
	}
	if got.SessionID == "" {
		t.Fatalf("no child spawned: %s", out)
	}

	if len(planStub.calls) != 1 {
		t.Fatalf("plan.SystemSet calls = %d, want 1", len(planStub.calls))
	}
	if planStub.calls[0].Text != block.PlanText {
		t.Errorf("plan body = %q, want %q", planStub.calls[0].Text, block.PlanText)
	}
	if planStub.calls[0].CurrentStep != "Explore" {
		t.Errorf("plan current_step = %q, want Explore", planStub.calls[0].CurrentStep)
	}
	if wbStub.calls != 1 {
		t.Errorf("whiteboard.SystemInit calls = %d, want 1", wbStub.calls)
	}
}

// ---------- spawn_wave ----------

// TestCallSpawnWave_BadRequest covers the empty-subagents and
// invalid-JSON refusals before any spawn happens.
func TestCallSpawnWave_BadRequest(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	out, _ := parent.callSpawnWave(us1WithSession(parent),
		json.RawMessage(`{"subagents":[]}`))
	mgr_assertErrorCode(t, out, "bad_request")

	out, _ = parent.callSpawnWave(us1WithSession(parent),
		json.RawMessage(`{not-json`))
	mgr_assertErrorCode(t, out, "bad_request")

	parent.childMu.Lock()
	defer parent.childMu.Unlock()
	if len(parent.children) != 0 {
		t.Errorf("parent.children = %d after spawn_wave validation failure, want 0", len(parent.children))
	}
}

// TestCallSpawnWave_PropagatesSpawnError verifies the wave tool
// surfaces a spawn-phase validation refusal as the underlying
// tool_error envelope (depth_exceeded here) without entering the
// wait phase.
func TestCallSpawnWave_PropagatesSpawnError(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()
	parent.deps.MaxDepth = 0
	parent.depth = 5

	out, err := parent.callSpawnWave(us1WithSession(parent),
		json.RawMessage(`{"wave_label":"explore","subagents":[{"task":"x"}]}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	mgr_assertErrorCode(t, out, "depth_exceeded")
}

// ---------- spawn_subagent ----------

// TestCallSpawnSubagent_Happy verifies the simplest path: a single
// child entry succeeds and the result names the new id at depth 1.
func TestCallSpawnSubagent_Happy(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	out, err := parent.callSpawnSubagent(us1WithSession(parent),
		json.RawMessage(`{"subagents":[{"task":"explore"}]}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}

	var got []spawnSubagentResult
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v\noutput=%s", err, out)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1; out=%s", len(got), out)
	}
	if got[0].SessionID == "" || got[0].Depth != 1 {
		t.Errorf("unexpected entry %+v", got[0])
	}
	// Child should be in parent.children — direct lookup, never a
	// tree walk: each session knows only its own immediate children.
	parent.childMu.Lock()
	_, inChildren := parent.children[got[0].SessionID]
	parent.childMu.Unlock()
	if !inChildren {
		t.Errorf("spawned child %q not in parent.children", got[0].SessionID)
	}
}

// TestCallSpawnSubagent_DepthExceeded asserts the validation refusal
// when parent.depth+1 > max_depth. The handler must return a
// tool_error JSON and NOT spawn anything.
func TestCallSpawnSubagent_DepthExceeded(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()
	// Force the cap so the next spawn would exceed it.
	parent.deps.MaxDepth = 0
	parent.depth = 5

	out, err := parent.callSpawnSubagent(us1WithSession(parent),
		json.RawMessage(`{"subagents":[{"task":"x"}]}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	mgr_assertErrorCode(t, out, "depth_exceeded")
	// No child registered.
	parent.childMu.Lock()
	defer parent.childMu.Unlock()
	if len(parent.children) != 0 {
		t.Errorf("parent.children = %d, want 0 after depth refusal", len(parent.children))
	}
}

// TestCallSpawnSubagent_BadRequest covers the empty-task and empty-
// batch refusals.
func TestCallSpawnSubagent_BadRequest(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	out, _ := parent.callSpawnSubagent(us1WithSession(parent),
		json.RawMessage(`{"subagents":[]}`))
	mgr_assertErrorCode(t, out, "bad_request")

	out, _ = parent.callSpawnSubagent(us1WithSession(parent),
		json.RawMessage(`{"subagents":[{"task":""}]}`))
	mgr_assertErrorCode(t, out, "bad_request")
}

// TestCallSpawnSubagent_BatchFailFast asserts that on the first
// invalid entry the whole batch fails — earlier entries are not
// spawned. (Validation runs before any parent.Spawn call.)
func TestCallSpawnSubagent_BatchFailFast(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	out, _ := parent.callSpawnSubagent(us1WithSession(parent),
		json.RawMessage(`{"subagents":[{"task":"good"},{"task":""}]}`))
	mgr_assertErrorCode(t, out, "bad_request")

	parent.childMu.Lock()
	defer parent.childMu.Unlock()
	if len(parent.children) != 0 {
		t.Errorf("parent.children = %d after fail-fast batch, want 0", len(parent.children))
	}
}

// stubSpawnHinter is a minimal SubagentSpawnHinter for the
// per-skill-intent wiring test. Implements the runtime's expected
// composition: it pretends to advertise one skill ("hugr-data") with
// one role ("explorer") whose Intent is "cheap".
type stubSpawnHinter struct {
	skill, role, intent string
	calls               int
}

func (h *stubSpawnHinter) Name() string { return "stub-hinter" }

func (h *stubSpawnHinter) SubagentSpawnHint(_ context.Context, _ extension.SessionState,
	skill, role string,
) (extension.SubagentSpawnHint, error) {
	h.calls++
	if skill == h.skill && role == h.role {
		return extension.SubagentSpawnHint{Intent: h.intent}, nil
	}
	return extension.SubagentSpawnHint{}, nil
}

// TestCallSpawnSubagent_RoleIntentOverride asserts that a skill
// manifest's role.intent surfaces as a per-session default-intent
// override on the spawned child. Phase-4.1d wiring: skill ext's
// SubagentSpawnHint → child.SetDefaultIntent.
func TestCallSpawnSubagent_RoleIntentOverride(t *testing.T) {
	hinter := &stubSpawnHinter{skill: "hugr-data", role: "explorer", intent: "cheap"}
	parent, cleanup := newTestParent(t, withTestExtensions(hinter))
	defer cleanup()

	out, err := parent.callSpawnSubagent(us1WithSession(parent),
		json.RawMessage(`{"subagents":[{"skill":"hugr-data","role":"explorer","task":"t"}]}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if hinter.calls == 0 {
		t.Errorf("SubagentSpawnHint never called — wiring broken")
	}
	var got []spawnSubagentResult
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v\nout=%s", err, out)
	}
	if len(got) != 1 {
		t.Fatalf("len(got)=%d, want 1; out=%s", len(got), out)
	}
	parent.childMu.Lock()
	child := parent.children[got[0].SessionID]
	parent.childMu.Unlock()
	if child == nil {
		t.Fatalf("child %q missing from parent.children", got[0].SessionID)
	}
	if want, got := "cheap", string(child.DefaultIntent()); got != want {
		t.Errorf("child.DefaultIntent() = %q, want %q", got, want)
	}
}

// TestCallSpawnSubagent_TierIntent_AppliesAtSpawn verifies the
// γ tier-intent default: when deps.TierIntents has an entry for
// the spawned child's tier, applyChildIntent uses it as the
// default before per-role overrides. Phase 4.2.2 §11.
func TestCallSpawnSubagent_TierIntent_AppliesAtSpawn(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()
	parent.deps.TierIntents = map[string]string{
		"mission": "cheap", // child is depth 1 → tier=mission
	}

	out, err := parent.callSpawnSubagent(us1WithSession(parent),
		json.RawMessage(`{"subagents":[{"task":"t"}]}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var got []spawnSubagentResult
	_ = json.Unmarshal(out, &got)
	parent.childMu.Lock()
	child := parent.children[got[0].SessionID]
	parent.childMu.Unlock()
	if child == nil {
		t.Fatalf("child missing")
	}
	if want, got := "cheap", string(child.DefaultIntent()); got != want {
		t.Errorf("child.DefaultIntent() = %q, want %q (tier-intent default)", got, want)
	}
}

// TestCallSpawnSubagent_RoleIntent_OverridesTierIntent verifies
// the override precedence: per-role intent from a skill manifest
// wins over the tier-intent default. Phase 4.2.2 §11.
func TestCallSpawnSubagent_RoleIntent_OverridesTierIntent(t *testing.T) {
	hinter := &stubSpawnHinter{skill: "hugr-data", role: "explorer", intent: "cheap"}
	parent, cleanup := newTestParent(t, withTestExtensions(hinter))
	defer cleanup()
	parent.deps.TierIntents = map[string]string{
		"mission": "default", // role's "cheap" must override this
	}

	out, err := parent.callSpawnSubagent(us1WithSession(parent),
		json.RawMessage(`{"subagents":[{"skill":"hugr-data","role":"explorer","task":"t"}]}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var got []spawnSubagentResult
	_ = json.Unmarshal(out, &got)
	parent.childMu.Lock()
	child := parent.children[got[0].SessionID]
	parent.childMu.Unlock()
	if want, got := "cheap", string(child.DefaultIntent()); got != want {
		t.Errorf("child.DefaultIntent() = %q, want %q (role override of tier intent)", got, want)
	}
}

// TestCallSpawnSubagent_NoIntent_KeepsParentDefault asserts the
// default path: when the skill role doesn't declare an intent, the
// child stays on IntentDefault — no spurious override.
func TestCallSpawnSubagent_NoIntent_KeepsParentDefault(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	out, err := parent.callSpawnSubagent(us1WithSession(parent),
		json.RawMessage(`{"subagents":[{"task":"t"}]}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var got []spawnSubagentResult
	_ = json.Unmarshal(out, &got)
	parent.childMu.Lock()
	child := parent.children[got[0].SessionID]
	parent.childMu.Unlock()
	if child == nil {
		t.Fatalf("child missing")
	}
	if got := string(child.DefaultIntent()); got != "default" {
		t.Errorf("child.DefaultIntent() = %q, want default", got)
	}
}

// TestCallSpawnSubagent_SessionGone verifies the closed-session guard
// — Post phase-4.1b-pre stage A handlers receive *Session directly so
// the only "no-caller" failure mode is calling against a session that
// has already terminated. The dispatcher will not normally route here
// after close, but the guard keeps the handler honest.
func TestCallSpawnSubagent_SessionGone(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()
	parent.MarkClosed()

	out, err := parent.callSpawnSubagent(us1WithSession(parent),
		json.RawMessage(`{"subagents":[{"task":"t"}]}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	mgr_assertErrorCode(t, out, "session_gone")
}

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
	// for it without waiting for real natural termination.
	out, err := parent.callSpawnSubagent(us1WithSession(parent),
		json.RawMessage(`{"subagents":[{"task":"t"}]}`))
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	var spawned []spawnSubagentResult
	_ = json.Unmarshal(out, &spawned)
	childID := spawned[0].SessionID
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
	out, err := parent.callSpawnSubagent(us1WithSession(parent),
		json.RawMessage(`{"subagents":[{"task":"explore catalog","role":"explorer"}]}`))
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	var spawned []spawnSubagentResult
	_ = json.Unmarshal(out, &spawned)
	childID := spawned[0].SessionID
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

// TestCallWaitSubagents_BadRequest covers the empty-ids guard.
func TestCallWaitSubagents_BadRequest(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()

	out, _ := parent.callWaitSubagents(us1WithSession(parent),
		json.RawMessage(`{"ids":[]}`))
	mgr_assertErrorCode(t, out, "bad_request")
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

// ---------- parent_context ----------

// TestCallParentContext_Filtering seeds a parent's events with a mix
// of types and asserts only user/assistant messages flow through.
func TestCallParentContext_Filtering(t *testing.T) {
	testStore := fixture.NewTestStore()
	parent, cleanup := newTestParent(t, withTestStore(testStore))
	defer cleanup()

	child, err := parent.Spawn(context.Background(), SpawnSpec{Task: "t"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	// Seed parent events: 1 user, 1 final assistant, 1 reasoning, 1
	// tool_call, 1 non-final assistant chunk.
	at := time.Now().Add(-time.Hour)
	mustAppend := func(et string, content string, meta map[string]any) {
		t.Helper()
		_ = testStore.AppendEvent(context.Background(), EventRow{
			ID:        "x",
			SessionID: parent.ID(),
			AgentID:   "a1",
			EventType: et,
			Author:    "u1",
			Content:   content,
			Metadata:  meta,
			CreatedAt: at,
		}, "")
		at = at.Add(time.Second)
	}
	mustAppend(string(protocol.KindUserMessage), "user-says", nil)
	mustAppend(string(protocol.KindAgentMessage), "assistant-final", map[string]any{"final": true, "consolidated": true})
	mustAppend(string(protocol.KindReasoning), "thinking", nil)
	mustAppend(string(protocol.KindToolCall), "tool", nil)
	mustAppend(string(protocol.KindAgentMessage), "assistant-mid", map[string]any{"final": false})

	args, _ := json.Marshal(parentContextInput{Limit: 20})
	out, err := child.callParentContext(extension.WithSessionState(context.Background(), child), args)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var got parentContextOutput
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Expect 2 messages: user-says + assistant-final. Reasoning,
	// tool_call, non-final assistant filtered out.
	if len(got.Messages) != 2 {
		t.Errorf("len = %d, want 2; got=%+v", len(got.Messages), got.Messages)
	}
	roles := make(map[string]string)
	for _, m := range got.Messages {
		roles[m.Role] = m.Content
	}
	if roles["user"] != "user-says" {
		t.Errorf("user msg = %q, want %q", roles["user"], "user-says")
	}
	if roles["assistant"] != "assistant-final" {
		t.Errorf("assistant msg = %q, want %q", roles["assistant"], "assistant-final")
	}
}

// TestCallParentContext_QueryAndTimeWindow combines substring + from
// filter.
func TestCallParentContext_QueryAndTimeWindow(t *testing.T) {
	testStore := fixture.NewTestStore()
	parent, cleanup := newTestParent(t, withTestStore(testStore))
	defer cleanup()

	child, err := parent.Spawn(context.Background(), SpawnSpec{Task: "t"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	rows := []struct {
		offset time.Duration
		text   string
	}{
		{0, "old apple message"},
		{time.Hour, "newer banana note"},
		{2 * time.Hour, "cherry payload here"},
	}
	for _, r := range rows {
		_ = testStore.AppendEvent(context.Background(), EventRow{
			ID: "x", SessionID: parent.ID(), AgentID: "a1",
			EventType: string(protocol.KindUserMessage),
			Content:   r.text, CreatedAt: base.Add(r.offset),
		}, "")
	}

	from := base.Add(30 * time.Minute).Format(time.RFC3339)
	args, _ := json.Marshal(parentContextInput{
		Query: "BANANA",
		From:  from,
	})
	out, err := child.callParentContext(extension.WithSessionState(context.Background(), child), args)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var got parentContextOutput
	_ = json.Unmarshal(out, &got)
	if len(got.Messages) != 1 || got.Messages[0].Content != "newer banana note" {
		t.Errorf("filtered messages = %+v, want only 'newer banana note'", got.Messages)
	}
}

// TestCallParentContext_NoParentForRoot — root sessions surface
// no_parent.
func TestCallParentContext_NoParentForRoot(t *testing.T) {
	root, cleanup := newTestParent(t)
	defer cleanup()

	out, err := root.callParentContext(extension.WithSessionState(context.Background(), root),
		json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	mgr_assertErrorCode(t, out, "no_parent")
}

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
