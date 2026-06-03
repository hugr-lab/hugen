package compactor

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/prompts"
)

// rendererState is a fakeState that returns a real prompts renderer —
// SummarizeSegment reads it via state.Prompts() and resolves the
// checkpoint window via state's tier.
type rendererState struct {
	*fakeState
	renderer *prompts.Renderer
}

func (s *rendererState) Prompts() *prompts.Renderer { return s.renderer }

// TestSummarizeSegment exercises the hide-time summariser plumbing:
// resolve the cheap model, render compactor/hide_brief, stream the
// brief back. The stub model echoes a canned brief (carrying a standing
// rule) so we assert the template renders + the call returns it.
func TestSummarizeSegment(t *testing.T) {
	mdl := &stubModel{summary: "op2023: 4 tables. RULE: validate every query before recording."}
	router := newStubRouter(t, mdl)
	ext := NewExtensionWithConfig(slog.Default(), DefaultConfig(), Deps{Router: router})
	st := &rendererState{fakeState: newFakeState("ses-sum"), renderer: productionRendererForCompactor(t)}

	entries := []HistoryEntry{
		{Seq: 5, Message: model.Message{Role: model.RoleTool,
			Content: "queries.md: VALIDATE every query with data-validate_graphql_query before recording"}},
		{Seq: 6, Message: model.Message{Role: model.RoleAssistant,
			ToolCalls: []model.ChunkToolCall{{ID: "c1", Name: "discovery-field_values", Args: map[string]any{"field": "x"}}}}},
	}
	brief, err := ext.SummarizeSegment(context.Background(), st, entries, "keep the validation rule")
	if err != nil {
		t.Fatalf("SummarizeSegment: %v", err)
	}
	if !strings.Contains(strings.ToLower(brief), "validate every query") {
		t.Fatalf("brief lost the standing rule: %q", brief)
	}
	if mdl.callCount() != 1 {
		t.Fatalf("summariser model called %d times, want 1", mdl.callCount())
	}
}

func TestSummarizeSegment_NoRouterErrors(t *testing.T) {
	ext := NewExtensionWithConfig(slog.Default(), DefaultConfig(), Deps{}) // nil Router
	st := &rendererState{fakeState: newFakeState("ses-nr"), renderer: productionRendererForCompactor(t)}
	if _, err := ext.SummarizeSegment(context.Background(), st,
		[]HistoryEntry{{Seq: 1, Message: model.Message{Role: model.RoleTool, Content: "x"}}}, ""); err == nil {
		t.Fatalf("SummarizeSegment with nil router should error (caller falls back)")
	}
}

// fakeSummarizer stubs SegmentSummarizer to assert the provider wires
// the note through + stores the returned brief as the placeholder.
type fakeSummarizer struct {
	brief   string
	err     error
	gotNote string
	called  bool
}

func (f *fakeSummarizer) SummarizeSegment(_ context.Context, _ extension.SessionState, _ []HistoryEntry, note string) (string, error) {
	f.called = true
	f.gotNote = note
	return f.brief, f.err
}

// TestHide_AutoSummaryWired pins that context:hide drives the
// summariser (seeding it with the agent note) and stores the returned
// brief as the checkpoint note / placeholder.
func TestHide_AutoSummaryWired(t *testing.T) {
	fs := &fakeSummarizer{brief: "AUTO: 4 tables; RULE validate queries before recording"}
	p := NewContextProvider(fs)
	st, cs := stateWithCheckpoints("ses-auto")
	ctx := ctxWith(st)
	appendEntry(cs, 1, model.RoleUser, "task")
	appendToolPair(cs, 2, "c1", "read_file", bigContent(50))
	cs.AddCheckpoint("raw discovery") // cp-1

	callContext(t, p, ctx, "context:hide", `{"cp_id":"cp-1","note":"remember the validation rule"}`)

	if !fs.called {
		t.Fatalf("hide did not invoke the summariser")
	}
	if fs.gotNote != "remember the validation rule" {
		t.Fatalf("agent note not seeded into summariser: %q", fs.gotNote)
	}
	cp, _ := cs.FindCheckpoint("cp-1")
	if cp.Note != fs.brief {
		t.Fatalf("checkpoint note = %q, want the auto brief %q", cp.Note, fs.brief)
	}
}

// TestHide_AutoSummaryFailsFallsBackToNote pins the best-effort
// contract: a summariser error never fails the hide — the agent note is
// kept verbatim instead.
func TestHide_AutoSummaryFailsFallsBackToNote(t *testing.T) {
	fs := &fakeSummarizer{err: errors.New("model down")}
	p := NewContextProvider(fs)
	st, cs := stateWithCheckpoints("ses-fb")
	ctx := ctxWith(st)
	appendEntry(cs, 1, model.RoleUser, "task")
	appendToolPair(cs, 2, "c1", "read_file", bigContent(50))
	cs.AddCheckpoint("raw discovery")

	res := callContext(t, p, ctx, "context:hide", `{"cp_id":"cp-1","note":"validate queries"}`)
	if res["ok"] != true {
		t.Fatalf("hide should succeed despite summariser failure: %+v", res)
	}
	cp, _ := cs.FindCheckpoint("cp-1")
	if cp.Note != "validate queries" {
		t.Fatalf("on summariser failure, note should fall back verbatim; got %q", cp.Note)
	}
}
