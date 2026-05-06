package session

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// TestUS2_PlanSurvivesRestart exercises phase-4-spec §13.2 #12: a
// plan written on one process boot is fully reconstituted on the
// next boot purely from session_events. We simulate the boot
// boundary by tearing down Manager1 and re-opening Manager2 against
// the same fakeStore, then Resume the session and read both the
// rendered system prompt and the plan_show output.
func TestUS2_PlanSurvivesRestart(t *testing.T) {
	store := newFakeStore()

	// Boot 1: open session, write a plan + 2 comments.
	mgr1 := newTestManager(t, store)
	ctx := context.Background()
	parent, _, err := mgr1.Open(ctx, OpenRequest{OwnerID: "alice"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	drainOutboxOnce(parent.Outbox())

	setArgs, _ := json.Marshal(planSetInput{Text: "investigate latency", CurrentStep: "scope"})
	if _, err := callPlanSet(us1WithSession(parent), parent, mgrToolHost(mgr1), setArgs); err != nil {
		t.Fatalf("plan_set: %v", err)
	}
	for _, txt := range []string{"checked headers", "instrumented handler"} {
		args, _ := json.Marshal(planCommentInput{Text: txt})
		if _, err := callPlanComment(us1WithSession(parent), parent, mgrToolHost(mgr1), args); err != nil {
			t.Fatalf("plan_comment: %v", err)
		}
	}
	parentID := parent.id
	mgr1.Stop(ctx) // graceful — writes nothing terminal.

	// Boot 2: fresh Manager + agent, same store.
	mgr2 := newTestManager(t, store)
	defer mgr2.Stop(ctx)

	resumed, err := mgr2.Resume(ctx, parentID)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}

	// Force lazy materialise so the in-memory plan is rebuilt from
	// events without waiting on a real user-message round-trip.
	if err := resumed.materialise(ctx); err != nil {
		t.Fatalf("materialise: %v", err)
	}

	// systemPrompt should now carry the plan block.
	prompt := resumed.systemPrompt(ctx)
	if !strings.Contains(prompt, "## Active plan") {
		t.Errorf("systemPrompt after restart missing plan block: %q", prompt)
	}
	if !strings.Contains(prompt, "investigate latency") {
		t.Errorf("systemPrompt missing body: %q", prompt)
	}

	// plan_show returns body + both retained comments.
	out, err := callPlanShow(us1WithSession(resumed), resumed, mgrToolHost(mgr2), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("plan_show: %v", err)
	}
	var got planShowOutput
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Active || got.Text != "investigate latency" {
		t.Errorf("show output = %+v, want active body", got)
	}
	if len(got.Comments) != 2 {
		t.Fatalf("Comments len = %d, want 2 after restart", len(got.Comments))
	}
	if got.Comments[0].Text != "checked headers" || got.Comments[1].Text != "instrumented handler" {
		t.Errorf("Comments = %+v, want chronological replay", got.Comments)
	}
}

// TestUS2_PlanSurvivesHistoryWindow guards the phase-4 promise that
// the plan is the model's anchor across history truncation: 60+
// synthetic user/agent messages exceed defaultHistoryWindow=50,
// but the plan block must still render at the top of the system
// prompt because its source is the full event log, not the
// windowed history.
func TestUS2_PlanSurvivesHistoryWindow(t *testing.T) {
	store := newFakeStore()
	mgr := newTestManager(t, store)
	defer mgr.Stop(context.Background())
	parent := us1OpenParent(t, mgr)

	setArgs, _ := json.Marshal(planSetInput{Text: "anchor body", CurrentStep: "step-1"})
	if _, err := callPlanSet(us1WithSession(parent), parent, mgrToolHost(mgr), setArgs); err != nil {
		t.Fatalf("plan_set: %v", err)
	}

	// Inject 60 user_message events directly through the store so
	// the windowed history fills past defaultHistoryWindow=50.
	at := time.Now().Add(-time.Hour)
	for i := 0; i < 60; i++ {
		err := store.AppendEvent(context.Background(), EventRow{
			ID:        "u-x",
			SessionID: parent.id,
			AgentID:   "a1",
			EventType: string(protocol.KindUserMessage),
			Author:    "u1",
			Content:   "msg",
			CreatedAt: at,
		}, "")
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		at = at.Add(time.Second)
	}

	// Force re-materialise via fresh Resume in a new manager so the
	// projection gets rebuilt against the inflated history.
	mgr.Stop(context.Background())
	mgr2 := newTestManager(t, store)
	defer mgr2.Stop(context.Background())
	resumed, err := mgr2.Resume(context.Background(), parent.id)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if err := resumed.materialise(context.Background()); err != nil {
		t.Fatalf("materialise: %v", err)
	}

	// History window is 50 most-recent — the plan_op event, which
	// is older than every seeded user_message, would be dropped if
	// the plan rebuilt from the windowed history. Asserting plan
	// presence in the prompt validates the "plan reads the full
	// event log" invariant.
	prompt := resumed.systemPrompt(context.Background())
	if !strings.Contains(prompt, "## Active plan") {
		t.Errorf("plan dropped under history truncation: prompt=%q", prompt)
	}
	if !strings.Contains(prompt, "anchor body") {
		t.Errorf("plan body missing under history truncation: prompt=%q", prompt)
	}
}

// TestUS2_PlanEndToEnd exercises §13.2 #11: plan_set then a sub-
// agent flow then a plan_comment then a plan_show. Asserts that
// orchestration tools and plan tools compose cleanly within one
// session.
func TestUS2_PlanEndToEnd(t *testing.T) {
	mgr := newTestManager(t, newFakeStore())
	defer mgr.Stop(context.Background())
	parent := us1OpenParent(t, mgr)

	// 1. Set the plan.
	setArgs, _ := json.Marshal(planSetInput{Text: "fan-out and merge", CurrentStep: "spawn"})
	if _, err := callPlanSet(us1WithSession(parent), parent, mgrToolHost(mgr), setArgs); err != nil {
		t.Fatalf("plan_set: %v", err)
	}

	// 2. Spawn a sub-agent (we don't need it to produce a real
	// result — just confirm tool composition works).
	spawnArgs, _ := json.Marshal(spawnSubagentInput{Subagents: []spawnEntry{{Task: "scout"}}})
	if _, err := callSpawnSubagent(us1WithSession(parent), parent, mgrToolHost(mgr), spawnArgs); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	drainOutboxOnce(parent.Outbox()) // subagent_started

	// 3. Comment after the spawn.
	cArgs, _ := json.Marshal(planCommentInput{Text: "scout dispatched", CurrentStep: "wait"})
	if _, err := callPlanComment(us1WithSession(parent), parent, mgrToolHost(mgr), cArgs); err != nil {
		t.Fatalf("plan_comment: %v", err)
	}

	// 4. Show; both ops landed.
	out, _ := callPlanShow(us1WithSession(parent), parent, mgrToolHost(mgr), json.RawMessage(`{}`))
	var got planShowOutput
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Active || got.Text != "fan-out and merge" {
		t.Errorf("show body = %+v", got)
	}
	if got.CurrentStep != "wait" {
		t.Errorf("CurrentStep = %q, want 'wait' (moved by comment)", got.CurrentStep)
	}
	if len(got.Comments) != 1 || got.Comments[0].Text != "scout dispatched" {
		t.Errorf("Comments = %+v, want single 'scout dispatched'", got.Comments)
	}
}
