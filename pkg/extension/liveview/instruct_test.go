package liveview

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/internal/fixture"
)

// TestPerTurnPrompt_EmptyWhenNoChildren — calling PerTurnPrompt
// on a session with no children produces no inject block.
func TestPerTurnPrompt_EmptyWhenNoChildren(t *testing.T) {
	ext := New(nil)
	state := fixture.NewTestSessionState("ses-root")
	if err := ext.InitState(context.Background(), state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	defer ext.CloseSession(context.Background(), state)

	if got := ext.PerTurnPrompt(context.Background(), state); got != "" {
		t.Errorf("PerTurnPrompt with no children = %q; want empty", got)
	}
}

// TestPerTurnPrompt_EmptyOnWorkerTier — workers (depth >= 2) skip
// the block regardless of cache contents; the gate is needed
// because notepad / liveview observe child frames but workers
// never legitimately have children of their own.
func TestPerTurnPrompt_EmptyOnWorkerTier(t *testing.T) {
	ext := New(nil)
	state := fixture.NewTestSessionState("ses-worker").WithDepth(2)
	if err := ext.InitState(context.Background(), state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	defer ext.CloseSession(context.Background(), state)

	v := fromState(state)
	if v == nil {
		t.Fatal("liveview state missing")
	}
	v.reportMu.Lock()
	v.children["ses-test-child"] = json.RawMessage(`{"lifecycle_state":"active"}`)
	v.reportMu.Unlock()

	if got := ext.PerTurnPrompt(context.Background(), state); got != "" {
		t.Errorf("worker tier returned non-empty inject: %q", got)
	}
}

// TestPerTurnPrompt_RendersParkedRowWithHint — a parked child
// produces an "Active sub-agents" block with the parked hint
// mentioning notify_subagent + subagent_dismiss. This is the
// load-bearing case the migration was added for.
func TestPerTurnPrompt_RendersParkedRowWithHint(t *testing.T) {
	ext := New(nil)
	state := fixture.NewTestSessionState("ses-root-1")
	if err := ext.InitState(context.Background(), state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	defer ext.CloseSession(context.Background(), state)

	v := fromState(state)
	if v == nil {
		t.Fatal("liveview state missing")
	}
	parkedAt := time.Now().UTC().Add(-12 * time.Second)
	payload, _ := json.Marshal(map[string]any{
		"session_id":      "ses-parked0001",
		"lifecycle_state": "awaiting_dismissal",
		"parked_at":       parkedAt,
	})
	v.reportMu.Lock()
	v.children["ses-parked0001"] = payload
	v.childMeta["ses-parked0001"] = childMetaEntry{
		Role:  "data-chatter",
		Skill: "data-chat",
		Task:  "top-3 customers by order value",
	}
	v.reportMu.Unlock()

	out := ext.PerTurnPrompt(context.Background(), state)
	if out == "" {
		t.Fatal("expected non-empty inject")
	}
	for _, want := range []string{
		"## Active sub-agents",
		"You have 1 direct sub-agent",
		"data-chatter · data-chat",
		"awaiting_dismissal",
		"notify_subagent",
		"subagent_dismiss",
		"top-3 customers by order value",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("inject missing %q; got:\n%s", want, out)
		}
	}
	// Parked-age render — exact "12s" since parkedAt was 12s ago.
	if !strings.Contains(out, "parked 1") {
		// Allow 11/12/13 second drift across the test run.
		t.Errorf("missing parked-age fragment; got:\n%s", out)
	}
}

// TestPerTurnPrompt_StableRowOrder — children sorted by id so two
// consecutive PerTurnPrompt calls on the same state produce the
// exact same bytes. Stability matters for provider-side prompt
// caches and adapter diff rendering.
func TestPerTurnPrompt_StableRowOrder(t *testing.T) {
	ext := New(nil)
	state := fixture.NewTestSessionState("ses-root-2")
	if err := ext.InitState(context.Background(), state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	defer ext.CloseSession(context.Background(), state)

	v := fromState(state)
	if v == nil {
		t.Fatal("liveview state missing")
	}
	v.reportMu.Lock()
	for _, id := range []string{"ses-bbbbbbbb", "ses-aaaaaaaa", "ses-cccccccc"} {
		v.children[id] = json.RawMessage(`{"lifecycle_state":"active"}`)
		v.childMeta[id] = childMetaEntry{Role: "data-chatter", Task: "q"}
	}
	v.reportMu.Unlock()

	first := ext.PerTurnPrompt(context.Background(), state)
	second := ext.PerTurnPrompt(context.Background(), state)
	if first != second {
		t.Errorf("two consecutive renders differ:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	// First row must be the lexicographically smallest id.
	idxA := strings.Index(first, "aaaaaaaa")
	idxB := strings.Index(first, "bbbbbbbb")
	idxC := strings.Index(first, "cccccccc")
	if idxA < 0 || idxB < 0 || idxC < 0 || !(idxA < idxB && idxB < idxC) {
		t.Errorf("rows not sorted by id; positions a=%d b=%d c=%d", idxA, idxB, idxC)
	}
}
