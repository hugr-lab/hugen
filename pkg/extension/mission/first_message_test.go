package mission

import (
	"strings"
	"testing"
)

func TestBuildWorkerFirstMessage_NilInputs(t *testing.T) {
	got := buildWorkerFirstMessage("do the thing", nil)
	if got != "do the thing" {
		t.Errorf("nil inputs: got %q, want bare task", got)
	}
}

func TestBuildWorkerFirstMessage_TrivialInputs(t *testing.T) {
	cases := []struct {
		name string
		in   any
	}{
		{"empty map", map[string]any{}},
		{"empty slice", []any{}},
		{"empty string", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildWorkerFirstMessage("task body", tc.in)
			if got != "task body" {
				t.Errorf("got %q, want bare task (trivial inputs should be skipped)", got)
			}
		})
	}
}

func TestBuildWorkerFirstMessage_NonTrivialInputsPrependsBlock(t *testing.T) {
	inputs := map[string]any{
		"file_path":     "~/Downloads/report.html",
		"output_format": "html",
	}
	got := buildWorkerFirstMessage("Generate the report.", inputs)
	if !strings.HasPrefix(got, "[Inputs from parent]\n") {
		t.Fatalf("first block must be [Inputs from parent], got: %q", got[:min(80, len(got))])
	}
	if !strings.Contains(got, `"file_path": "~/Downloads/report.html"`) {
		t.Errorf("file_path value missing from block: %q", got)
	}
	if !strings.Contains(got, `"output_format": "html"`) {
		t.Errorf("output_format value missing from block: %q", got)
	}
	if !strings.Contains(got, "[Task]\nGenerate the report.") {
		t.Errorf("[Task] section missing original task body: %q", got)
	}
	idxIn := strings.Index(got, "[Inputs from parent]")
	idxTask := strings.Index(got, "[Task]")
	if idxIn < 0 || idxTask < 0 || idxIn > idxTask {
		t.Errorf("ordering wrong: [Inputs from parent] at %d, [Task] at %d", idxIn, idxTask)
	}
}

// Workers grep for the literal "[Inputs from parent]" string — the
// label MUST match the worker_contract.tmpl section exactly.
func TestBuildWorkerFirstMessage_BlockLabelMatchesContractTemplate(t *testing.T) {
	got := buildWorkerFirstMessage("t", map[string]any{"k": "v"})
	if !strings.Contains(got, "[Inputs from parent]") {
		t.Fatal(`block label must be exactly "[Inputs from parent]" (literal match required by worker contract)`)
	}
}

// Task may arrive with a [Resolved depends_on] prefix the executor
// prepended. The [Inputs from parent] block should land ABOVE the
// task (which still carries its resolved-deps prefix inside).
func TestBuildWorkerFirstMessage_PreservesResolvedDependsOnPrefix(t *testing.T) {
	task := "[Resolved depends_on]\n<analyst@extract (role: data-analyst, status: ok)>\n{...body...}\n\nSave the html under inputs.file_path."
	got := buildWorkerFirstMessage(task, map[string]any{"file_path": "/Users/v/x.html"})
	idxIn := strings.Index(got, "[Inputs from parent]")
	idxDeps := strings.Index(got, "[Resolved depends_on]")
	idxTask := strings.Index(got, "[Task]")
	if idxIn < 0 || idxTask < 0 || idxDeps < 0 {
		t.Fatalf("missing required sections in %q", got)
	}
	if !(idxIn < idxTask && idxTask < idxDeps) {
		t.Errorf("section order should be Inputs → Task → Resolved-deps; got Inputs=%d Task=%d Deps=%d", idxIn, idxTask, idxDeps)
	}
}
