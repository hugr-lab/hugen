package session

import (
	"strings"
	"testing"
)

// TestComposeChildFirstMessage_NoInputs verifies the spawn surface
// degrades to verbatim `task` when callers don't pass inputs (the
// common case for /skill list and one-off spawns from tests).
func TestComposeChildFirstMessage_NoInputs(t *testing.T) {
	for _, in := range []any{
		nil,
		map[string]any{},
		[]any{},
		"",
	} {
		got := composeChildFirstMessage("do the thing", in, "")
		if got != "do the thing" {
			t.Errorf("inputs=%v: got %q, want verbatim task", in, got)
		}
	}
}

// TestComposeChildFirstMessage_WithInputs verifies the
// planner→worker handoff template lands the inputs block above
// the task body so the worker sees both. Catches regressions in
// the prefix wording (workers grep for "[Inputs from parent]" via
// the SKILL prose).
func TestComposeChildFirstMessage_WithInputs(t *testing.T) {
	inputs := map[string]any{
		"module":      "op2023",
		"tables":      []string{"op2023_providers"},
		"query_draft": "query { op2023 { providers { npi } } }",
		"file_path":   "~/Downloads/x.html",
	}
	got := composeChildFirstMessage("Generate HTML report.", inputs, "")
	if !strings.HasPrefix(got, "[Inputs from parent]\n") {
		t.Errorf("missing inputs block prefix: %q", got)
	}
	for _, want := range []string{
		"\"module\": \"op2023\"",
		"\"query_draft\": \"query",
		"\"file_path\": \"~/Downloads/x.html\"",
		"[Task]\nGenerate HTML report.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in composed message:\n%s", want, got)
		}
	}
}

// TestComposeChildFirstMessage_NestedShapes covers the typical
// post-json.Unmarshal shape (map[string]any with nested slice +
// map) — both top-level keys and nested values must surface.
func TestComposeChildFirstMessage_NestedShapes(t *testing.T) {
	inputs := map[string]any{
		"data_source": "op2023",
		"filters":     map[string]any{"limit": 50, "ordered": true},
		"tables":      []any{"t1", "t2"},
	}
	got := composeChildFirstMessage("task body", inputs, "")
	for _, want := range []string{
		"\"data_source\": \"op2023\"",
		"\"limit\": 50",
		"\"ordered\": true",
		"\"t1\"",
		"\"t2\"",
		"[Task]\ntask body",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in composed message:\n%s", want, got)
		}
	}
}

// TestComposeChildFirstMessage_WithWhiteboard verifies the third
// block — pre-rendered whiteboard digest — lands at the TOP of
// the composed message (before inputs, before task). When both
// inputs and whiteboard are present, order is fixed: Whiteboard
// → Inputs → Task (read top-down).
func TestComposeChildFirstMessage_WithWhiteboard(t *testing.T) {
	wb := "(2 messages on board, active since 13:38)\n\n#1 @13:40 from planner:\n  Validated query: { op2023 { providers } }\n\n#2 @14:25 from data-analyst:\n  Top providers parquet at data/x.parquet"
	got := composeChildFirstMessage("Build report", map[string]any{"k": "v"}, wb)
	for _, want := range []string{
		"[Whiteboard]\n(2 messages on board",
		"#1 @13:40 from planner",
		"#2 @14:25 from data-analyst",
		"[Inputs from parent]",
		"\"k\": \"v\"",
		"[Task]\nBuild report",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in composed message:\n%s", want, got)
		}
	}
	// Order check — Whiteboard must appear before Inputs, Inputs before Task.
	idxWB := strings.Index(got, "[Whiteboard]")
	idxIn := strings.Index(got, "[Inputs from parent]")
	idxTask := strings.Index(got, "[Task]")
	if !(idxWB < idxIn && idxIn < idxTask) {
		t.Errorf("block order wrong: wb=%d inputs=%d task=%d (want strictly increasing)",
			idxWB, idxIn, idxTask)
	}
}

// TestComposeChildFirstMessage_OnlyWhiteboard verifies the
// whiteboard-only path: no inputs, just whiteboard + task. Skips
// the inputs block entirely.
func TestComposeChildFirstMessage_OnlyWhiteboard(t *testing.T) {
	wb := "(1 message on board, active since 13:38)\n\n#1 @13:40 from planner:\n  hello"
	got := composeChildFirstMessage("Pick up", nil, wb)
	if !strings.HasPrefix(got, "[Whiteboard]\n") {
		t.Errorf("wb block missing at top: %s", got)
	}
	if strings.Contains(got, "[Inputs from parent]") {
		t.Errorf("inputs block leaked despite nil inputs: %s", got)
	}
	if !strings.Contains(got, "[Task]\nPick up") {
		t.Errorf("task block missing: %s", got)
	}
}
