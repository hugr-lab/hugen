package mission

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

func TestExtension_InitState(t *testing.T) {
	ext := NewExtension(Config{AgentID: "ag-1"})
	state := newFakeState("ses-1")

	if err := ext.InitState(context.Background(), state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	m := FromState(state)
	if m == nil {
		t.Fatal("FromState: nil after InitState")
	}
	if m.Handoffs == nil {
		t.Error("MissionState.Handoffs is nil")
	}
	if m.CurrentWave() != "" {
		t.Errorf("CurrentWave = %q, want empty", m.CurrentWave())
	}
}

func TestExtension_FromState_WalksParent(t *testing.T) {
	mission := newFakeState("mis-1")
	worker := newFakeState("wrk-1")
	worker.parent = mission

	NewExtension(Config{}).InitState(context.Background(), mission) // skipcq

	if m := FromState(worker); m == nil {
		t.Fatal("FromState(worker) = nil, want parent's MissionState reachable via Parent()")
	}
}

func TestExtension_OnChildFrame_HandoffIngest(t *testing.T) {
	ext := NewExtension(Config{})
	state := newFakeState("mis-1")
	ext.InitState(context.Background(), state) // skipcq

	m := FromState(state)
	m.BeginWave("wave-1")
	m.RegisterWorker("wrk-1", workerCursor{
		Name: "w1",
		Role: "echo",
	})

	final := &protocol.AgentMessage{
		Payload: protocol.AgentMessagePayload{
			Text:         "Some prose.\n\n```handoff\n{\"status\":\"ok\",\"body\":\"hello\",\"memory_summary\":\"echoed back\"}\n```\n",
			Final:        true,
			Consolidated: true,
		},
	}
	ext.OnChildFrame(context.Background(), state, "wrk-1", final)

	got, ok := m.Handoffs.Get("w1@wave-1")
	if !ok {
		t.Fatal("Handoffs.Get(w1@wave-1): not found after OnChildFrame")
	}
	if got.Status != "ok" {
		t.Errorf("Status = %q, want ok", got.Status)
	}
	if got.MemorySummary != "echoed back" {
		t.Errorf("MemorySummary = %q, want 'echoed back'", got.MemorySummary)
	}
	if got.Subagent.SessionID != "wrk-1" || got.Subagent.Name != "w1" {
		t.Errorf("Subagent = %+v, want SessionID=wrk-1 Name=w1", got.Subagent)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt unset")
	}
}

func TestExtension_OnChildFrame_UnknownChildIgnored(t *testing.T) {
	ext := NewExtension(Config{})
	state := newFakeState("mis-1")
	ext.InitState(context.Background(), state) // skipcq

	m := FromState(state)
	m.BeginWave("w1")
	// No RegisterWorker — frame should be ignored.

	final := &protocol.AgentMessage{
		Payload: protocol.AgentMessagePayload{
			Text:         "```handoff\n{\"status\":\"ok\",\"body\":\"x\"}\n```",
			Final:        true,
			Consolidated: true,
		},
	}
	ext.OnChildFrame(context.Background(), state, "stranger", final)

	if m.Handoffs.Len() != 0 {
		t.Errorf("Handoffs.Len = %d, want 0 (unknown child must not produce a ref)", m.Handoffs.Len())
	}
}

func TestExtension_OnChildFrame_NoCurrentWaveIgnored(t *testing.T) {
	ext := NewExtension(Config{})
	state := newFakeState("mis-1")
	ext.InitState(context.Background(), state) // skipcq

	m := FromState(state)
	m.RegisterWorker("wrk-1", workerCursor{Name: "w1"})
	// No BeginWave — frame ignored.

	final := &protocol.AgentMessage{
		Payload: protocol.AgentMessagePayload{
			Text:         "```handoff\n{\"status\":\"ok\",\"body\":\"x\"}\n```",
			Final:        true,
			Consolidated: true,
		},
	}
	ext.OnChildFrame(context.Background(), state, "wrk-1", final)
	if m.Handoffs.Len() != 0 {
		t.Errorf("Handoffs.Len = %d, want 0 (no active wave)", m.Handoffs.Len())
	}
}

func TestExtension_OnChildFrame_ErrorRecordedAsErrorHandoff(t *testing.T) {
	ext := NewExtension(Config{})
	state := newFakeState("mis-1")
	ext.InitState(context.Background(), state) // skipcq
	m := FromState(state)
	m.BeginWave("w1")
	m.RegisterWorker("wrk-1", workerCursor{Name: "w1", Role: "echo"})

	errFrame := &protocol.Error{
		Payload: protocol.ErrorPayload{
			Code:    "model_error",
			Message: "stream broke",
		},
	}
	ext.OnChildFrame(context.Background(), state, "wrk-1", errFrame)

	got, ok := m.Handoffs.Get("w1@w1")
	if !ok {
		t.Fatal("error frame must produce a synthetic error handoff")
	}
	if got.Status != "error" {
		t.Errorf("Status = %q, want error", got.Status)
	}
	if !strings.Contains(got.Reason, "stream broke") {
		t.Errorf("Reason = %q, want substring 'stream broke'", got.Reason)
	}
}

func TestExtension_OnChildFrame_ParseFailureRecordedAsError(t *testing.T) {
	ext := NewExtension(Config{})
	state := newFakeState("mis-1")
	ext.InitState(context.Background(), state) // skipcq
	m := FromState(state)
	m.BeginWave("w1")
	m.RegisterWorker("wrk-1", workerCursor{Name: "w1"})

	frame := &protocol.AgentMessage{
		Payload: protocol.AgentMessagePayload{
			Text:         "I produced no handoff, sorry.",
			Final:        true,
			Consolidated: true,
		},
	}
	ext.OnChildFrame(context.Background(), state, "wrk-1", frame)

	got, ok := m.Handoffs.Get("w1@w1")
	if !ok {
		t.Fatal("missing-handoff worker must produce a synthetic error entry")
	}
	if got.Status != "error" {
		t.Errorf("Status = %q, want error", got.Status)
	}
}

func TestExtension_ReportStatus_NilWhenNoState(t *testing.T) {
	ext := NewExtension(Config{})
	state := newFakeState("mis-1")
	// No InitState call — handle absent.
	if data := ext.ReportStatus(context.Background(), state); data != nil {
		t.Errorf("ReportStatus without InitState = %s, want nil", data)
	}
}

func TestExtension_ReportStatus_NilWhenInactive(t *testing.T) {
	ext := NewExtension(Config{})
	state := newFakeState("mis-1")
	ext.InitState(context.Background(), state) // skipcq

	if data := ext.ReportStatus(context.Background(), state); data != nil {
		t.Errorf("ReportStatus with idle mission state = %s, want nil", data)
	}
}

func TestExtension_ReportStatus_PayloadOnActiveWave(t *testing.T) {
	ext := NewExtension(Config{})
	state := newFakeState("mis-1")
	ext.InitState(context.Background(), state) // skipcq
	m := FromState(state)
	m.BeginWave("w1")
	m.Handoffs.Put(Handoff{Ref: "w1@w1", Status: "ok", CreatedAt: time.Now()})

	data := ext.ReportStatus(context.Background(), state)
	if data == nil {
		t.Fatal("ReportStatus = nil, want payload on active wave")
	}
	var payload struct {
		Plan         PlanState `json:"plan"`
		ActiveWave   string    `json:"active_wave"`
		HandoffCount int       `json:"handoff_count"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("ReportStatus payload unmarshal: %v", err)
	}
	if payload.ActiveWave != "w1" {
		t.Errorf("ActiveWave = %q, want w1", payload.ActiveWave)
	}
	if payload.HandoffCount != 1 {
		t.Errorf("HandoffCount = %d, want 1", payload.HandoffCount)
	}
}

func TestExtension_ToolCalls_GetHandoff(t *testing.T) {
	ext := NewExtension(Config{})
	state := newFakeState("mis-1")
	ext.InitState(context.Background(), state) // skipcq
	m := FromState(state)
	m.Handoffs.Put(Handoff{
		Ref:       "x@w",
		Kind:      KindHandoff,
		Status:    "ok",
		Body:      "data",
		CreatedAt: time.Now(),
	})

	// callGetHandoff reads state from ctx. The fake state goes
	// into ctx via the same machinery production uses.
	ctx := contextWithState(state)

	args, _ := json.Marshal(getHandoffInput{Ref: "x@w"})
	out, err := ext.Call(ctx, "mission:get_handoff", args)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(string(out), `"ref":"x@w"`) {
		t.Errorf("Call get_handoff = %s, want ref field", out)
	}

	// Missing ref → not_found envelope.
	missing, _ := json.Marshal(getHandoffInput{Ref: "missing@w"})
	out2, err := ext.Call(ctx, "mission:get_handoff", missing)
	if err != nil {
		t.Fatalf("Call missing: %v", err)
	}
	if !strings.Contains(string(out2), "not_found") {
		t.Errorf("Call missing = %s, want not_found envelope", out2)
	}

	// Malformed ref → bad_request.
	bad, _ := json.Marshal(getHandoffInput{Ref: "no-at-sign"})
	out3, err := ext.Call(ctx, "mission:get_handoff", bad)
	if err != nil {
		t.Fatalf("Call bad ref: %v", err)
	}
	if !strings.Contains(string(out3), "bad_request") {
		t.Errorf("Call bad ref = %s, want bad_request envelope", out3)
	}
}

func TestExtension_ToolCalls_Finish(t *testing.T) {
	ext := NewExtension(Config{})
	state := newFakeState("mis-1")
	ext.InitState(context.Background(), state) // skipcq
	ctx := contextWithState(state)

	// Missing reason → bad_request.
	out, err := ext.Call(ctx, "mission:finish", []byte(`{}`))
	if err != nil {
		t.Fatalf("Call empty: %v", err)
	}
	if !strings.Contains(string(out), "bad_request") {
		t.Errorf("Call empty = %s, want bad_request", out)
	}

	// Happy path.
	out, err = ext.Call(ctx, "mission:finish", []byte(`{"reason":"completed","text":"done"}`))
	if err != nil {
		t.Fatalf("Call finish: %v", err)
	}
	if !strings.Contains(string(out), `"ok":true`) {
		t.Errorf("Call finish = %s, want ok envelope", out)
	}
}
