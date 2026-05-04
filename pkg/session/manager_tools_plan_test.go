package session

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// ---------- plan_set ----------

// TestCallPlanSet_Happy: a fresh session writes a plan; the
// in-memory projection reflects body + pointer; the persisted
// event has the right shape.
func TestCallPlanSet_Happy(t *testing.T) {
	store := newFakeStore()
	mgr := newTestManager(t, store)
	defer mgr.ShutdownAll(context.Background())
	parent := us1OpenParent(t, mgr)

	args, _ := json.Marshal(planSetInput{Text: "investigate cache", CurrentStep: "scope"})
	out, err := callPlanSet(us1WithSession(parent), mgr, args)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var got planOKOutput
	if err := json.Unmarshal(out, &got); err != nil || !got.OK {
		t.Fatalf("plan_set output = %s err=%v", out, err)
	}

	parent.planMu.Lock()
	plan := parent.plan
	parent.planMu.Unlock()
	if !plan.Active || plan.Text != "investigate cache" || plan.CurrentStep != "scope" {
		t.Errorf("in-memory plan = %+v, want active body+pointer", plan)
	}

	// Persisted event check.
	events, _ := store.ListEvents(context.Background(), parent.id, ListEventsOpts{})
	found := false
	for _, ev := range events {
		if ev.EventType == string(protocol.KindPlanOp) {
			found = true
			if ev.Metadata["op"] != "set" {
				t.Errorf("event op = %v, want set", ev.Metadata["op"])
			}
			if ev.Metadata["text"] != "investigate cache" {
				t.Errorf("event text = %v, want body", ev.Metadata["text"])
			}
		}
	}
	if !found {
		t.Errorf("no plan_op event persisted")
	}
}

// TestCallPlanSet_BadRequest covers missing-text refusal.
func TestCallPlanSet_BadRequest(t *testing.T) {
	mgr := newTestManager(t, newFakeStore())
	defer mgr.ShutdownAll(context.Background())
	parent := us1OpenParent(t, mgr)

	out, _ := callPlanSet(us1WithSession(parent), mgr,
		json.RawMessage(`{}`))
	mgr_assertErrorCode(t, out, "bad_request")
}

// TestCallPlanSet_MissingSessionContext — wiring guard.
func TestCallPlanSet_MissingSessionContext(t *testing.T) {
	mgr := newTestManager(t, newFakeStore())
	defer mgr.ShutdownAll(context.Background())

	out, err := callPlanSet(context.Background(), mgr,
		json.RawMessage(`{"text":"x"}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	mgr_assertErrorCode(t, out, "missing_session_context")
}

// ---------- plan_comment ----------

// TestCallPlanComment_Happy: after plan_set, plan_comment appends a
// comment and updates current_step preservation correctly.
func TestCallPlanComment_Happy(t *testing.T) {
	mgr := newTestManager(t, newFakeStore())
	defer mgr.ShutdownAll(context.Background())
	parent := us1OpenParent(t, mgr)

	setArgs, _ := json.Marshal(planSetInput{Text: "body", CurrentStep: "a"})
	if _, err := callPlanSet(us1WithSession(parent), mgr, setArgs); err != nil {
		t.Fatalf("set: %v", err)
	}

	// Comment with no current_step → should preserve "a".
	cArgs, _ := json.Marshal(planCommentInput{Text: "noted"})
	if _, err := callPlanComment(us1WithSession(parent), mgr, cArgs); err != nil {
		t.Fatalf("comment: %v", err)
	}

	parent.planMu.Lock()
	plan := parent.plan
	parent.planMu.Unlock()
	if len(plan.Comments) != 1 || plan.Comments[0].Text != "noted" {
		t.Errorf("Comments = %+v, want one 'noted'", plan.Comments)
	}
	if plan.CurrentStep != "a" {
		t.Errorf("CurrentStep = %q, want 'a' (preserved)", plan.CurrentStep)
	}

	// Second comment with explicit pointer → should move it.
	cArgs2, _ := json.Marshal(planCommentInput{Text: "moved", CurrentStep: "b"})
	if _, err := callPlanComment(us1WithSession(parent), mgr, cArgs2); err != nil {
		t.Fatalf("comment2: %v", err)
	}
	parent.planMu.Lock()
	plan = parent.plan
	parent.planMu.Unlock()
	if plan.CurrentStep != "b" {
		t.Errorf("CurrentStep = %q, want 'b' (moved)", plan.CurrentStep)
	}
	if len(plan.Comments) != 2 {
		t.Errorf("Comments len = %d, want 2", len(plan.Comments))
	}
}

// TestCallPlanComment_NoActivePlan: comment without prior set must
// surface no_active_plan.
func TestCallPlanComment_NoActivePlan(t *testing.T) {
	mgr := newTestManager(t, newFakeStore())
	defer mgr.ShutdownAll(context.Background())
	parent := us1OpenParent(t, mgr)

	args, _ := json.Marshal(planCommentInput{Text: "x"})
	out, _ := callPlanComment(us1WithSession(parent), mgr, args)
	mgr_assertErrorCode(t, out, "no_active_plan")
}

// TestCallPlanComment_BadRequest covers missing-text refusal.
func TestCallPlanComment_BadRequest(t *testing.T) {
	mgr := newTestManager(t, newFakeStore())
	defer mgr.ShutdownAll(context.Background())
	parent := us1OpenParent(t, mgr)

	// Without prior set, the bad_request check fires before
	// no_active_plan since unmarshal fails on missing "text" only
	// when the JSON decoder sees an empty string. Use a
	// deliberately-malformed shape to hit the bad_request branch.
	out, _ := callPlanComment(us1WithSession(parent), mgr,
		json.RawMessage(`{"text":"["`)) // truncated → unmarshal error
	mgr_assertErrorCode(t, out, "bad_request")
}

// ---------- plan_show ----------

// TestCallPlanShow_Inactive returns active=false on a fresh session.
func TestCallPlanShow_Inactive(t *testing.T) {
	mgr := newTestManager(t, newFakeStore())
	defer mgr.ShutdownAll(context.Background())
	parent := us1OpenParent(t, mgr)

	out, err := callPlanShow(us1WithSession(parent), mgr, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	var got planShowOutput
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Active {
		t.Errorf("expected active=false on fresh session; got %+v", got)
	}
}

// TestCallPlanShow_Roundtrip: set + 2 comments → show returns body
// + pointer + both comments.
func TestCallPlanShow_Roundtrip(t *testing.T) {
	mgr := newTestManager(t, newFakeStore())
	defer mgr.ShutdownAll(context.Background())
	parent := us1OpenParent(t, mgr)

	setArgs, _ := json.Marshal(planSetInput{Text: "v1", CurrentStep: "phase-1"})
	_, _ = callPlanSet(us1WithSession(parent), mgr, setArgs)

	for _, txt := range []string{"first", "second"} {
		cArgs, _ := json.Marshal(planCommentInput{Text: txt})
		_, _ = callPlanComment(us1WithSession(parent), mgr, cArgs)
	}

	out, _ := callPlanShow(us1WithSession(parent), mgr, json.RawMessage(`{}`))
	var got planShowOutput
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Active || got.Text != "v1" || got.CurrentStep != "phase-1" {
		t.Errorf("unexpected show output: %+v", got)
	}
	if len(got.Comments) != 2 ||
		got.Comments[0].Text != "first" || got.Comments[1].Text != "second" {
		t.Errorf("Comments = %+v, want first then second", got.Comments)
	}
}

// ---------- plan_clear ----------

// TestCallPlanClear: after clear the projection is inactive and a
// subsequent show returns active=false.
func TestCallPlanClear(t *testing.T) {
	store := newFakeStore()
	mgr := newTestManager(t, store)
	defer mgr.ShutdownAll(context.Background())
	parent := us1OpenParent(t, mgr)

	setArgs, _ := json.Marshal(planSetInput{Text: "tmp"})
	_, _ = callPlanSet(us1WithSession(parent), mgr, setArgs)

	if _, err := callPlanClear(us1WithSession(parent), mgr, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("clear: %v", err)
	}

	parent.planMu.Lock()
	if parent.plan.Active {
		t.Errorf("plan still active after clear: %+v", parent.plan)
	}
	parent.planMu.Unlock()

	out, _ := callPlanShow(us1WithSession(parent), mgr, json.RawMessage(`{}`))
	if !strings.Contains(string(out), `"active":false`) {
		t.Errorf("show after clear = %s, want active:false", out)
	}
}

// TestPlanRendersInSystemPrompt: setting a plan injects the block
// into the next systemPrompt() call.
func TestPlanRendersInSystemPrompt(t *testing.T) {
	mgr := newTestManager(t, newFakeStore())
	defer mgr.ShutdownAll(context.Background())
	parent := us1OpenParent(t, mgr)

	setArgs, _ := json.Marshal(planSetInput{Text: "investigate latency", CurrentStep: "instrument"})
	_, _ = callPlanSet(us1WithSession(parent), mgr, setArgs)

	prompt := parent.systemPrompt(context.Background())
	if !strings.Contains(prompt, "## Active plan") {
		t.Errorf("systemPrompt missing plan block: %q", prompt)
	}
	if !strings.Contains(prompt, "Current focus: instrument") {
		t.Errorf("systemPrompt missing current focus: %q", prompt)
	}
	if !strings.Contains(prompt, "investigate latency") {
		t.Errorf("systemPrompt missing body: %q", prompt)
	}
}
