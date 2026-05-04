package session

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// us1OpenParent opens a fresh root session and drains the
// session_opened frame so the test reads only events it asserts on.
func us1OpenParent(t *testing.T, mgr *Manager) *Session {
	t.Helper()
	parent, _, err := mgr.Open(context.Background(), OpenRequest{OwnerID: "alice"})
	if err != nil {
		t.Fatalf("open parent: %v", err)
	}
	drainOutboxOnce(parent.Outbox())
	return parent
}

// us1WithSession is the standard test ctx that pretends a tool
// dispatcher has already wired the calling session via
// dispatchToolCall.
func us1WithSession(parent *Session) context.Context {
	return WithSession(context.Background(), parent)
}

// ---------- spawn_subagent ----------

// TestCallSpawnSubagent_Happy verifies the simplest path: a single
// child entry succeeds and the result names the new id at depth 1.
func TestCallSpawnSubagent_Happy(t *testing.T) {
	mgr := newTestManager(t, newFakeStore())
	defer mgr.ShutdownAll(context.Background())
	parent := us1OpenParent(t, mgr)

	out, err := callSpawnSubagent(us1WithSession(parent), mgr,
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
	// Child should be in parent.children.
	if parent.FindDescendant(got[0].SessionID) == nil {
		t.Errorf("spawned child %q not in parent's tree", got[0].SessionID)
	}
}

// TestCallSpawnSubagent_DepthExceeded asserts the validation refusal
// when parent.depth+1 > max_depth. The handler must return a
// tool_error JSON and NOT spawn anything.
func TestCallSpawnSubagent_DepthExceeded(t *testing.T) {
	mgr := newTestManager(t, newFakeStore())
	defer mgr.ShutdownAll(context.Background())
	parent := us1OpenParent(t, mgr)
	// Force the cap so the next spawn would exceed it.
	parent.deps.maxDepth = 0
	parent.depth = 5

	out, err := callSpawnSubagent(us1WithSession(parent), mgr,
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
	mgr := newTestManager(t, newFakeStore())
	defer mgr.ShutdownAll(context.Background())
	parent := us1OpenParent(t, mgr)

	out, _ := callSpawnSubagent(us1WithSession(parent), mgr,
		json.RawMessage(`{"subagents":[]}`))
	mgr_assertErrorCode(t, out, "bad_request")

	out, _ = callSpawnSubagent(us1WithSession(parent), mgr,
		json.RawMessage(`{"subagents":[{"task":""}]}`))
	mgr_assertErrorCode(t, out, "bad_request")
}

// TestCallSpawnSubagent_BatchFailFast asserts that on the first
// invalid entry the whole batch fails — earlier entries are not
// spawned. (Validation runs before any parent.Spawn call.)
func TestCallSpawnSubagent_BatchFailFast(t *testing.T) {
	mgr := newTestManager(t, newFakeStore())
	defer mgr.ShutdownAll(context.Background())
	parent := us1OpenParent(t, mgr)

	out, _ := callSpawnSubagent(us1WithSession(parent), mgr,
		json.RawMessage(`{"subagents":[{"task":"good"},{"task":""}]}`))
	mgr_assertErrorCode(t, out, "bad_request")

	parent.childMu.Lock()
	defer parent.childMu.Unlock()
	if len(parent.children) != 0 {
		t.Errorf("parent.children = %d after fail-fast batch, want 0", len(parent.children))
	}
}

// TestCallSpawnSubagent_NoSession verifies the missing-context guard
// — the dispatcher would normally inject the session via
// WithSession, so a missing entry is a wiring bug.
func TestCallSpawnSubagent_NoSession(t *testing.T) {
	mgr := newTestManager(t, newFakeStore())
	defer mgr.ShutdownAll(context.Background())

	out, err := callSpawnSubagent(context.Background(), mgr,
		json.RawMessage(`{"subagents":[{"task":"t"}]}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	mgr_assertErrorCode(t, out, "missing_session_context")
}

// ---------- wait_subagents ----------

// TestCallWaitSubagents_Happy_LiveResult drives wait_subagents
// through a synthetic SubagentResult Submit. The test runs the call
// in a goroutine and feeds the result Frame into the parent's inbox
// via Submit; the routing layer hands it to activeToolFeed.
func TestCallWaitSubagents_Happy_LiveResult(t *testing.T) {
	mgr := newTestManager(t, newFakeStore())
	defer mgr.ShutdownAll(context.Background())
	parent := us1OpenParent(t, mgr)

	// Spawn a real child so the id exists; we'll synthesise a result
	// for it without waiting for real natural termination.
	out, err := callSpawnSubagent(us1WithSession(parent), mgr,
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
		out, err := callWaitSubagents(us1WithSession(parent), mgr, args)
		done <- res{out: out, err: err}
	}()

	// Give wait_subagents time to register the feed.
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for parent.activeToolFeed.Load() == nil {
		select {
		case <-time.After(20 * time.Millisecond):
		case <-deadline.C:
			t.Fatal("activeToolFeed never registered")
		}
	}

	// Now submit the synthetic result. It rides through routeInbound
	// → RouteToolFeed → feed.Feed → wait_subagents.
	result := protocol.NewSubagentResult(parent.id, childID, parent.agent.Participant(),
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

// TestCallWaitSubagents_CachedShortCircuit pre-seeds parent's events
// with a SubagentResult, then calls wait_subagents — it must return
// immediately from drainCachedSubagentResults without ever
// registering an activeToolFeed.
func TestCallWaitSubagents_CachedShortCircuit(t *testing.T) {
	store := newFakeStore()
	mgr := newTestManager(t, store)
	defer mgr.ShutdownAll(context.Background())
	parent := us1OpenParent(t, mgr)

	cached := protocol.NewSubagentResult(parent.id, "child-cached", parent.agent.Participant(),
		protocol.SubagentResultPayload{
			SessionID: "child-cached",
			Result:    "from-store",
			Reason:    protocol.TerminationCompleted,
		})
	row, summary, _ := FrameToEventRow(cached, mgr.agent.ID())
	if err := store.AppendEvent(context.Background(), row, summary); err != nil {
		t.Fatalf("seed: %v", err)
	}

	args, _ := json.Marshal(waitSubagentsInput{IDs: []string{"child-cached"}})
	out, err := callWaitSubagents(us1WithSession(parent), mgr, args)
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
	if parent.activeToolFeed.Load() != nil {
		t.Error("activeToolFeed left registered after fully-cached drain")
	}
}

// TestCallWaitSubagents_BadRequest covers the empty-ids guard.
func TestCallWaitSubagents_BadRequest(t *testing.T) {
	mgr := newTestManager(t, newFakeStore())
	defer mgr.ShutdownAll(context.Background())
	parent := us1OpenParent(t, mgr)

	out, _ := callWaitSubagents(us1WithSession(parent), mgr,
		json.RawMessage(`{"ids":[]}`))
	mgr_assertErrorCode(t, out, "bad_request")
}

// ---------- subagent_runs ----------

// TestCallSubagentRuns_Happy seeds a child's events and verifies
// pagination + next_since_seq cursor.
func TestCallSubagentRuns_Happy(t *testing.T) {
	store := newFakeStore()
	mgr := newTestManager(t, store)
	defer mgr.ShutdownAll(context.Background())
	parent := us1OpenParent(t, mgr)

	child, err := parent.Spawn(context.Background(), SpawnSpec{Task: "t"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	// Seed five additional events on the child.
	for i := 0; i < 5; i++ {
		_ = store.AppendEvent(context.Background(), EventRow{
			ID:        "ev-x",
			SessionID: child.id,
			AgentID:   "a1",
			EventType: string(protocol.KindAgentMessage),
			Author:    "a1",
			Content:   "msg",
			CreatedAt: time.Now(),
		}, "")
	}

	args, _ := json.Marshal(subagentRunsInput{
		SessionID: child.id,
		Limit:     3,
	})
	out, err := callSubagentRuns(us1WithSession(parent), mgr, args)
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
	store := newFakeStore()
	mgr := newTestManager(t, store)
	defer mgr.ShutdownAll(context.Background())
	parent := us1OpenParent(t, mgr)

	// Create a row that's not a child of parent.
	other := SessionRow{ID: "ses-other", AgentID: "a1", Status: StatusActive,
		ParentSessionID: "ses-different-parent"}
	_ = store.OpenSession(context.Background(), other)

	args, _ := json.Marshal(subagentRunsInput{SessionID: "ses-other"})
	out, _ := callSubagentRuns(us1WithSession(parent), mgr, args)
	mgr_assertErrorCode(t, out, "not_a_child")

	// Unknown session also rejected.
	args, _ = json.Marshal(subagentRunsInput{SessionID: "ses-unknown"})
	out, _ = callSubagentRuns(us1WithSession(parent), mgr, args)
	mgr_assertErrorCode(t, out, "session_not_found")
}

// TestCallSubagentRuns_HardCap clamps the requested limit to
// subagentRunsHardCap (500). The test just verifies the clamp by
// sending limit > cap and asserting the call doesn't panic / err
// — pagination correctness is covered above.
func TestCallSubagentRuns_HardCap(t *testing.T) {
	mgr := newTestManager(t, newFakeStore())
	defer mgr.ShutdownAll(context.Background())
	parent := us1OpenParent(t, mgr)

	child, err := parent.Spawn(context.Background(), SpawnSpec{Task: "t"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	args, _ := json.Marshal(subagentRunsInput{
		SessionID: child.id,
		Limit:     5000, // request beyond cap
	})
	out, err := callSubagentRuns(us1WithSession(parent), mgr, args)
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
	store := newFakeStore()
	mgr := newTestManager(t, store)
	defer mgr.ShutdownAll(context.Background())
	parent := us1OpenParent(t, mgr)

	child, err := parent.Spawn(context.Background(), SpawnSpec{Task: "t"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	drainOutboxOnce(parent.Outbox()) // subagent_started

	args, _ := json.Marshal(subagentCancelInput{
		SessionID: child.id,
		Reason:    "user wants out",
	})
	out, err := callSubagentCancel(us1WithSession(parent), mgr, args)
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
	events, _ := store.ListEvents(context.Background(), child.id, ListEventsOpts{})
	wanted := protocol.TerminationSubagentCancelPrefix + "user wants out"
	if !containsKindWithReason(events, protocol.KindSessionTerminated, wanted) {
		t.Errorf("child terminated with wrong reason; events=%v", kindsWithReasons(events))
	}
}

// TestCallSubagentCancel_NotAChild blocks the cross-tree cancel
// path by ensuring not_a_child surfaces when the target isn't in
// the caller's children.
func TestCallSubagentCancel_NotAChild(t *testing.T) {
	store := newFakeStore()
	mgr := newTestManager(t, store)
	defer mgr.ShutdownAll(context.Background())
	parent := us1OpenParent(t, mgr)

	// Sibling root with no parent relationship.
	other, _, err := mgr.Open(context.Background(), OpenRequest{OwnerID: "bob"})
	if err != nil {
		t.Fatalf("open other: %v", err)
	}

	args, _ := json.Marshal(subagentCancelInput{SessionID: other.id})
	out, _ := callSubagentCancel(us1WithSession(parent), mgr, args)
	mgr_assertErrorCode(t, out, "not_a_child")
}

// TestCallSubagentCancel_Idempotent calls cancel twice; the second
// should still return ok=true even though the child has already
// exited.
func TestCallSubagentCancel_Idempotent(t *testing.T) {
	mgr := newTestManager(t, newFakeStore())
	defer mgr.ShutdownAll(context.Background())
	parent := us1OpenParent(t, mgr)

	child, err := parent.Spawn(context.Background(), SpawnSpec{Task: "t"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	args, _ := json.Marshal(subagentCancelInput{
		SessionID: child.id,
		Reason:    "first",
	})
	if _, err := callSubagentCancel(us1WithSession(parent), mgr, args); err != nil {
		t.Fatalf("first cancel: %v", err)
	}
	<-child.Done()

	out, err := callSubagentCancel(us1WithSession(parent), mgr, args)
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
	store := newFakeStore()
	mgr := newTestManager(t, store)
	defer mgr.ShutdownAll(context.Background())
	parent := us1OpenParent(t, mgr)

	child, err := parent.Spawn(context.Background(), SpawnSpec{Task: "t"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	// Seed parent events: 1 user, 1 final assistant, 1 reasoning, 1
	// tool_call, 1 non-final assistant chunk.
	at := time.Now().Add(-time.Hour)
	mustAppend := func(et string, content string, meta map[string]any) {
		t.Helper()
		_ = store.AppendEvent(context.Background(), EventRow{
			ID:        "x",
			SessionID: parent.id,
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
	mustAppend(string(protocol.KindAgentMessage), "assistant-final", map[string]any{"final": true})
	mustAppend(string(protocol.KindReasoning), "thinking", nil)
	mustAppend(string(protocol.KindToolCall), "tool", nil)
	mustAppend(string(protocol.KindAgentMessage), "assistant-mid", map[string]any{"final": false})

	args, _ := json.Marshal(parentContextInput{Limit: 20})
	out, err := callParentContext(WithSession(context.Background(), child), mgr, args)
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
	store := newFakeStore()
	mgr := newTestManager(t, store)
	defer mgr.ShutdownAll(context.Background())
	parent := us1OpenParent(t, mgr)

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
		_ = store.AppendEvent(context.Background(), EventRow{
			ID: "x", SessionID: parent.id, AgentID: "a1",
			EventType: string(protocol.KindUserMessage),
			Content:   r.text, CreatedAt: base.Add(r.offset),
		}, "")
	}

	from := base.Add(30 * time.Minute).Format(time.RFC3339)
	args, _ := json.Marshal(parentContextInput{
		Query: "BANANA",
		From:  from,
	})
	out, err := callParentContext(WithSession(context.Background(), child), mgr, args)
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
	mgr := newTestManager(t, newFakeStore())
	defer mgr.ShutdownAll(context.Background())
	root := us1OpenParent(t, mgr)

	out, err := callParentContext(WithSession(context.Background(), root), mgr,
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
