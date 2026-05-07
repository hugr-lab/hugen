package plan

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

// callCtx builds a dispatch ctx with the session state attached —
// the shape Session.dispatchToolCall would produce.
func callCtx(state extension.SessionState) context.Context {
	return extension.WithSessionState(context.Background(), state)
}

// newReadyExt builds a plan Extension with InitState already run on
// a fresh TestSessionState. Returns the ext + the state for emit
// inspection.
func newReadyExt(t *testing.T) (*Extension, *fixture.TestSessionState) {
	t.Helper()
	ext := NewExtension("agent-test")
	state := fixture.NewTestSessionState("ses-test")
	if err := ext.InitState(context.Background(), state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	return ext, state
}

// errResponse extracts the {"error":{"code":...}} envelope from a
// raw JSON response.
func errResponse(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var resp toolErrorResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode error response: %v (raw=%s)", err, raw)
	}
	return resp.Error.Code
}

// ---------- plan:set ----------

func TestCallSet_Happy(t *testing.T) {
	ext, state := newReadyExt(t)

	args, _ := json.Marshal(setInput{Text: "investigate cache", CurrentStep: "scope"})
	out, err := ext.Call(callCtx(state), "plan:set", args)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var ok okOutput
	if err := json.Unmarshal(out, &ok); err != nil || !ok.OK {
		t.Fatalf("set output = %s err=%v", out, err)
	}

	h := FromState(state)
	snap := h.Snapshot()
	if !snap.Active || snap.Text != "investigate cache" || snap.CurrentStep != "scope" {
		t.Errorf("snapshot = %+v, want active body+pointer", snap)
	}

	// Persisted frame.
	emitted := state.Emitted()
	if len(emitted) != 1 {
		t.Fatalf("emitted = %d, want 1", len(emitted))
	}
	pf, isPlanOp := emitted[0].(*protocol.PlanOp)
	if !isPlanOp {
		t.Fatalf("frame type = %T, want *PlanOp", emitted[0])
	}
	if pf.Payload.Op != "set" || pf.Payload.Text != "investigate cache" {
		t.Errorf("payload = %+v", pf.Payload)
	}
	if pf.Author().ID != "agent-test" {
		t.Errorf("author = %+v", pf.Author())
	}
}

func TestCallSet_BadRequest_Empty(t *testing.T) {
	ext, state := newReadyExt(t)
	out, _ := ext.Call(callCtx(state), "plan:set", json.RawMessage(`{}`))
	if got := errResponse(t, out); got != "bad_request" {
		t.Errorf("code = %q, want bad_request", got)
	}
}

func TestCallSet_BadRequest_Unmarshal(t *testing.T) {
	ext, state := newReadyExt(t)
	out, _ := ext.Call(callCtx(state), "plan:set", json.RawMessage(`{"text":1}`))
	if got := errResponse(t, out); got != "bad_request" {
		t.Errorf("code = %q, want bad_request", got)
	}
}

// TestCallSet_NoState — dispatch ctx without a state attached
// surfaces session_gone.
func TestCallSet_NoState(t *testing.T) {
	ext := NewExtension("a1")
	out, _ := ext.Call(context.Background(), "plan:set", json.RawMessage(`{"text":"x"}`))
	if got := errResponse(t, out); got != "session_gone" {
		t.Errorf("code = %q, want session_gone", got)
	}
}

// ---------- plan:comment ----------

func TestCallComment_Happy(t *testing.T) {
	ext, state := newReadyExt(t)
	ctx := callCtx(state)

	if _, err := ext.Call(ctx, "plan:set",
		mustMarshal(t, setInput{Text: "body", CurrentStep: "a"})); err != nil {
		t.Fatalf("set: %v", err)
	}

	// Comment with no current_step preserves "a".
	if _, err := ext.Call(ctx, "plan:comment",
		mustMarshal(t, commentInput{Text: "noted"})); err != nil {
		t.Fatalf("comment: %v", err)
	}

	snap := FromState(state).Snapshot()
	if len(snap.Comments) != 1 || snap.Comments[0].Text != "noted" {
		t.Errorf("Comments = %+v, want one 'noted'", snap.Comments)
	}
	if snap.CurrentStep != "a" {
		t.Errorf("CurrentStep = %q, want 'a' (preserved)", snap.CurrentStep)
	}

	// Second comment with explicit pointer moves it.
	if _, err := ext.Call(ctx, "plan:comment",
		mustMarshal(t, commentInput{Text: "moved", CurrentStep: "b"})); err != nil {
		t.Fatalf("comment2: %v", err)
	}
	snap = FromState(state).Snapshot()
	if snap.CurrentStep != "b" {
		t.Errorf("CurrentStep = %q, want 'b' (moved)", snap.CurrentStep)
	}
	if len(snap.Comments) != 2 {
		t.Errorf("Comments len = %d, want 2", len(snap.Comments))
	}
}

func TestCallComment_NoActivePlan(t *testing.T) {
	ext, state := newReadyExt(t)
	out, _ := ext.Call(callCtx(state), "plan:comment",
		mustMarshal(t, commentInput{Text: "x"}))
	if got := errResponse(t, out); got != "no_active_plan" {
		t.Errorf("code = %q, want no_active_plan", got)
	}
}

func TestCallComment_BadRequest(t *testing.T) {
	ext, state := newReadyExt(t)
	out, _ := ext.Call(callCtx(state), "plan:comment",
		json.RawMessage(`{"text":"`)) // truncated → unmarshal error
	if got := errResponse(t, out); got != "bad_request" {
		t.Errorf("code = %q, want bad_request", got)
	}
}

// ---------- plan:show ----------

func TestCallShow_Inactive(t *testing.T) {
	ext, state := newReadyExt(t)
	out, err := ext.Call(callCtx(state), "plan:show", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	var got showOutput
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Active {
		t.Errorf("expected active=false on fresh handle; got %+v", got)
	}
}

func TestCallShow_Roundtrip(t *testing.T) {
	ext, state := newReadyExt(t)
	ctx := callCtx(state)

	_, _ = ext.Call(ctx, "plan:set", mustMarshal(t, setInput{Text: "v1", CurrentStep: "phase-1"}))
	for _, txt := range []string{"first", "second"} {
		_, _ = ext.Call(ctx, "plan:comment", mustMarshal(t, commentInput{Text: txt}))
	}

	out, _ := ext.Call(ctx, "plan:show", json.RawMessage(`{}`))
	var got showOutput
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Active || got.Text != "v1" || got.CurrentStep != "phase-1" {
		t.Errorf("show = %+v", got)
	}
	if len(got.Comments) != 2 ||
		got.Comments[0].Text != "first" || got.Comments[1].Text != "second" {
		t.Errorf("Comments = %+v, want first then second", got.Comments)
	}
}

// ---------- plan:clear ----------

func TestCallClear(t *testing.T) {
	ext, state := newReadyExt(t)
	ctx := callCtx(state)

	_, _ = ext.Call(ctx, "plan:set", mustMarshal(t, setInput{Text: "tmp"}))
	if _, err := ext.Call(ctx, "plan:clear", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("clear: %v", err)
	}

	if FromState(state).Snapshot().Active {
		t.Errorf("plan still active after clear: %+v", FromState(state).Snapshot())
	}

	out, _ := ext.Call(ctx, "plan:show", json.RawMessage(`{}`))
	if !strings.Contains(string(out), `"active":false`) {
		t.Errorf("show after clear = %s, want active:false", out)
	}
}

// ---------- Advertiser ----------

func TestAdvertiseSystemPrompt_Active(t *testing.T) {
	ext, state := newReadyExt(t)
	_, _ = ext.Call(callCtx(state), "plan:set",
		mustMarshal(t, setInput{Text: "investigate latency", CurrentStep: "instrument"}))

	prompt := ext.AdvertiseSystemPrompt(context.Background(), state)
	if !strings.Contains(prompt, "## Active plan") {
		t.Errorf("missing plan block: %q", prompt)
	}
	if !strings.Contains(prompt, "Current focus: instrument") {
		t.Errorf("missing current focus: %q", prompt)
	}
	if !strings.Contains(prompt, "investigate latency") {
		t.Errorf("missing body: %q", prompt)
	}
}

func TestAdvertiseSystemPrompt_Inactive(t *testing.T) {
	ext, state := newReadyExt(t)
	if got := ext.AdvertiseSystemPrompt(context.Background(), state); got != "" {
		t.Errorf("inactive prompt = %q, want empty", got)
	}
}

// ---------- Recovery ----------

// TestRecover_RebuildsProjection: a fresh handle (no in-memory plan)
// fed a slice of plan_op event rows replays them via Project so the
// snapshot matches the persistent state. Mirrors the restart-resume
// invariant from phase-4-spec §13.2 #12.
func TestRecover_RebuildsProjection(t *testing.T) {
	ext, state := newReadyExt(t)

	now := time.Now().UTC()
	events := []store.EventRow{
		{
			SessionID: "ses-test",
			EventType: string(protocol.KindPlanOp),
			CreatedAt: now,
			Metadata: map[string]any{
				"op":           "set",
				"text":         "investigate latency",
				"current_step": "scope",
			},
		},
		{
			SessionID: "ses-test",
			EventType: string(protocol.KindPlanOp),
			CreatedAt: now.Add(time.Second),
			Metadata: map[string]any{
				"op":   "comment",
				"text": "checked headers",
			},
		},
		{
			SessionID: "ses-test",
			EventType: string(protocol.KindPlanOp),
			CreatedAt: now.Add(2 * time.Second),
			Metadata: map[string]any{
				"op":   "comment",
				"text": "instrumented handler",
			},
		},
	}

	if err := ext.Recover(context.Background(), state, events); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	snap := FromState(state).Snapshot()
	if !snap.Active || snap.Text != "investigate latency" {
		t.Errorf("snapshot = %+v, want rebuilt active body", snap)
	}
	if snap.CurrentStep != "scope" {
		t.Errorf("CurrentStep = %q, want 'scope'", snap.CurrentStep)
	}
	if len(snap.Comments) != 2 ||
		snap.Comments[0].Text != "checked headers" ||
		snap.Comments[1].Text != "instrumented handler" {
		t.Errorf("Comments = %+v, want chronological replay", snap.Comments)
	}

	// Advertiser surface still renders the rebuilt block.
	prompt := ext.AdvertiseSystemPrompt(context.Background(), state)
	if !strings.Contains(prompt, "investigate latency") {
		t.Errorf("prompt missing rebuilt body: %q", prompt)
	}
}

// TestRecover_ClearedTerminates: a clear after set yields an inactive
// projection — Project's "latest boundary wins" rule.
func TestRecover_ClearedTerminates(t *testing.T) {
	ext, state := newReadyExt(t)

	now := time.Now().UTC()
	events := []store.EventRow{
		{
			SessionID: "ses-test",
			EventType: string(protocol.KindPlanOp),
			CreatedAt: now,
			Metadata: map[string]any{"op": "set", "text": "tmp"},
		},
		{
			SessionID: "ses-test",
			EventType: string(protocol.KindPlanOp),
			CreatedAt: now.Add(time.Second),
			Metadata: map[string]any{"op": "clear"},
		},
	}

	if err := ext.Recover(context.Background(), state, events); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	if FromState(state).Snapshot().Active {
		t.Errorf("plan must be inactive after clear")
	}
}

// TestRecover_IgnoresUnrelatedRows: rows of other event kinds are
// skipped — Recovery only cares about plan_op.
func TestRecover_IgnoresUnrelatedRows(t *testing.T) {
	ext, state := newReadyExt(t)

	now := time.Now().UTC()
	events := []store.EventRow{
		{EventType: string(protocol.KindUserMessage), CreatedAt: now, Content: "noise"},
		{
			EventType: string(protocol.KindPlanOp),
			CreatedAt: now.Add(time.Second),
			Metadata:  map[string]any{"op": "set", "text": "anchor"},
		},
		{EventType: string(protocol.KindAgentMessage), CreatedAt: now.Add(2 * time.Second), Content: "more noise"},
	}

	if err := ext.Recover(context.Background(), state, events); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	snap := FromState(state).Snapshot()
	if !snap.Active || snap.Text != "anchor" {
		t.Errorf("snapshot = %+v, want active 'anchor' body", snap)
	}
}

// ---------- ToolProvider catalogue ----------

func TestList_FourTools(t *testing.T) {
	ext := NewExtension("a1")
	tools, err := ext.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := map[string]string{
		"plan:set":     PermObjectWrite,
		"plan:comment": PermObjectWrite,
		"plan:show":    PermObjectRead,
		"plan:clear":   PermObjectWrite,
	}
	if len(tools) != len(want) {
		t.Fatalf("len(tools) = %d, want %d", len(tools), len(want))
	}
	for _, tt := range tools {
		perm, ok := want[tt.Name]
		if !ok {
			t.Errorf("unexpected tool: %q", tt.Name)
			continue
		}
		if tt.PermissionObject != perm {
			t.Errorf("tool %q permission = %q, want %q", tt.Name, tt.PermissionObject, perm)
		}
		if tt.Provider != "plan" {
			t.Errorf("tool %q provider = %q, want plan", tt.Name, tt.Provider)
		}
	}
}

// mustMarshal panics on encode failure — fine for test setup.
func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
