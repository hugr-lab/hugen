package models

import (
	"encoding/json"
	"testing"

	"github.com/hugr-lab/query-engine/types"

	"github.com/hugr-lab/hugen/pkg/model"
)

func TestMessagesToHugrJSON_RoleMapping(t *testing.T) {
	cases := []struct {
		in       string
		wantRole string
	}{
		{model.RoleUser, "user"},
		{model.RoleAssistant, "assistant"},
		{"model", "assistant"},
		{model.RoleSystem, "system"},
		{model.RoleTool, "tool"},
		{"function", "tool"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			out, err := messagesToHugrJSON([]model.Message{{Role: c.in, Content: "hi"}})
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if len(out) != 1 {
				t.Fatalf("len=%d, want 1", len(out))
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(out[0]), &m); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if m["role"] != c.wantRole {
				t.Errorf("role=%v, want %s", m["role"], c.wantRole)
			}
			if m["content"] != "hi" {
				t.Errorf("content=%v, want %q", m["content"], "hi")
			}
		})
	}
}

func TestMessagesToHugrJSON_RejectsEmptyRole(t *testing.T) {
	if _, err := messagesToHugrJSON([]model.Message{{Content: "x"}}); err == nil {
		t.Fatal("expected error for empty role")
	}
}

func TestNormalizeToolArgs(t *testing.T) {
	tcs := []struct {
		name string
		in   any
		want any
	}{
		{"nil → empty object", nil, map[string]any{}},
		{"empty string → empty object", "", map[string]any{}},
		{"already an object", map[string]any{"x": 1}, map[string]any{"x": 1}},
		{"json string", `{"a":2}`, map[string]any{"a": float64(2)}},
		{"non-json string wrapped", "hello", map[string]any{"_raw": "hello"}},
		{"list wrapped", []any{1, 2}, map[string]any{"_raw": "[1,2]"}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeToolArgs(tc.in)
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(tc.want)
			if string(gotJSON) != string(wantJSON) {
				t.Errorf("got %s, want %s", gotJSON, wantJSON)
			}
		})
	}
}

func TestMessagesToHugrJSON_ToolCallNilArgsBecomesObject(t *testing.T) {
	out, err := messagesToHugrJSON([]model.Message{{
		Role: model.RoleAssistant,
		ToolCalls: []model.ChunkToolCall{
			{ID: "call_1", Name: "session:wait_subagents", Args: nil},
		},
	}})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out[0]), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	calls, ok := m["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("tool_calls = %v", m["tool_calls"])
	}
	first, _ := calls[0].(map[string]any)
	args, ok := first["arguments"].(map[string]any)
	if !ok {
		t.Errorf("arguments = %T (%v), want map[string]any", first["arguments"], first["arguments"])
	}
	if len(args) != 0 {
		t.Errorf("arguments = %v, want empty object", args)
	}
}

func TestMessagesToHugrJSON_PreservesToolCallID(t *testing.T) {
	out, err := messagesToHugrJSON([]model.Message{
		{Role: model.RoleTool, Content: "ok", ToolCallID: "call_42"},
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("len=%d, want 1", len(out))
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out[0]), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["tool_call_id"] != "call_42" {
		t.Errorf("tool_call_id=%v, want call_42", m["tool_call_id"])
	}
}

func TestMessagesToHugrJSON_MultiTurn(t *testing.T) {
	out, err := messagesToHugrJSON([]model.Message{
		{Role: model.RoleUser, Content: "what is 2+2?"},
		{Role: model.RoleAssistant, Content: "4"},
		{Role: model.RoleUser, Content: "thanks"},
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("len=%d, want 3", len(out))
	}
	wantRoles := []string{"user", "assistant", "user"}
	for i, want := range wantRoles {
		var m map[string]any
		if err := json.Unmarshal([]byte(out[i]), &m); err != nil {
			t.Fatalf("unmarshal %d: %v", i, err)
		}
		if m["role"] != want {
			t.Errorf("[%d] role=%v, want %s", i, m["role"], want)
		}
	}
}

func TestToolsToHugrJSON_DefaultsParameters(t *testing.T) {
	out, err := toolsToHugrJSON([]model.Tool{
		{Name: "search", Description: "Search the web"},
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("len=%d, want 1", len(out))
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out[0]), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["name"] != "search" {
		t.Errorf("name=%v", m["name"])
	}
	params, ok := m["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("parameters not an object: %T", m["parameters"])
	}
	if params["type"] != "object" {
		t.Errorf("default schema missing type=object, got %v", params)
	}
}

func TestToolsToHugrJSON_PreservesSchema(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"q": map[string]any{"type": "string"},
		},
		"required": []any{"q"},
	}
	out, err := toolsToHugrJSON([]model.Tool{
		{Name: "search", Schema: schema},
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out[0]), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, _ := json.Marshal(m["parameters"])
	want, _ := json.Marshal(schema)
	if string(got) != string(want) {
		t.Errorf("schema drift:\n got: %s\nwant: %s", got, want)
	}
}

// TestStreamEventToChunk asserts the Hugr-event → Chunk mapping the
// pump goroutine relies on. (Phase 2 / R-Plan-23 / SC-012 — no
// duplicate content at end-of-turn.)
func TestStreamEventToChunk_ContentDelta(t *testing.T) {
	ch, ok := streamEventToChunk(streamEvent("content_delta", "Hello"))
	if !ok {
		t.Fatal("expected ok=true")
	}
	if ch.Content == nil || *ch.Content != "Hello" {
		t.Errorf("Content=%v, want Hello", ch.Content)
	}
	if ch.Reasoning != nil {
		t.Errorf("Reasoning leaked: %v", *ch.Reasoning)
	}
	if ch.Final {
		t.Errorf("Final must be false on a delta")
	}
}

func TestStreamEventToChunk_Reasoning(t *testing.T) {
	ch, ok := streamEventToChunk(streamEvent("reasoning", "thinking..."))
	if !ok {
		t.Fatal("expected ok=true")
	}
	if ch.Reasoning == nil || *ch.Reasoning != "thinking..." {
		t.Errorf("Reasoning=%v", ch.Reasoning)
	}
	if ch.Content != nil {
		t.Errorf("Content leaked")
	}
}

func TestStreamEventToChunk_EmptyContentSkipped(t *testing.T) {
	if _, ok := streamEventToChunk(streamEvent("content_delta", "")); ok {
		t.Error("empty content_delta must be skipped")
	}
	if _, ok := streamEventToChunk(streamEvent("reasoning", "")); ok {
		t.Error("empty reasoning must be skipped")
	}
}

func TestStreamEventToChunk_UnknownEventSkipped(t *testing.T) {
	if _, ok := streamEventToChunk(streamEvent("totally_new", "x")); ok {
		t.Error("unknown event must be skipped silently")
	}
}

// streamEvent is a tiny builder for types.LLMStreamEvent.
func streamEvent(typ, content string) types.LLMStreamEvent {
	return types.LLMStreamEvent{Type: typ, Content: content}
}
