package liveview

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/prompts"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// budgetTestState lets the test pin every dimension the
// aggregator reads from SessionState.
type budgetTestState struct {
	id              string
	values          sync.Map
	usage           *protocol.TokenUsage
	toolTokens      int
}

func (s *budgetTestState) SessionID() string                      { return s.id }
func (s *budgetTestState) SubagentName() string                   { return "" }
func (s *budgetTestState) Role() string                           { return "" }
func (s *budgetTestState) Skill() string                          { return "" }
func (s *budgetTestState) Depth() int                             { return 0 }
func (s *budgetTestState) Parent() (extension.SessionState, bool) { return nil, false }
func (s *budgetTestState) Children() []extension.SessionState     { return nil }
func (s *budgetTestState) Tools() *tool.ToolManager               { return nil }
func (s *budgetTestState) Prompts() *prompts.Renderer             { return nil }
func (s *budgetTestState) Value(name string) (any, bool) {
	v, ok := s.values.Load(name)
	return v, ok
}
func (s *budgetTestState) SetValue(name string, value any) { s.values.Store(name, value) }
func (s *budgetTestState) Emit(_ context.Context, _ protocol.Frame) error { return nil }
func (s *budgetTestState) IsClosed() bool                                 { return false }
func (s *budgetTestState) Submit(_ context.Context, _ protocol.Frame) <-chan struct{} {
	return nil
}
func (s *budgetTestState) OutboxOnly(_ context.Context, _ protocol.Frame) error { return nil }
func (s *budgetTestState) Extensions() []extension.Extension                    { return nil }
func (s *budgetTestState) RequestInquiry(_ context.Context, _ protocol.InquiryRequestPayload) (*protocol.InquiryResponse, error) {
	return nil, nil
}
func (s *budgetTestState) ToolCatalogTokens(_ context.Context) int { return s.toolTokens }
func (s *budgetTestState) SessionUsage() *protocol.TokenUsage      { return s.usage }

// TestBuildContextBudget_AggregatesEveryDimension drives the
// happy-path: session usage + tools + history + per-extension
// advertise + skill split. Each dimension lands in the right
// slot.
func TestBuildContextBudget_AggregatesEveryDimension(t *testing.T) {
	state := &budgetTestState{
		id:         "ses-budget",
		usage:      &protocol.TokenUsage{PromptTokens: 12000, CompletionTokens: 3000},
		toolTokens: 2400,
	}
	exts := map[string]json.RawMessage{
		"compactor": json.RawMessage(`{"history_tokens":1500,"advertise_tokens":600}`),
		"notepad":   json.RawMessage(`{"advertise_tokens":400}`),
		"plan":      json.RawMessage(`{"advertise_tokens":200}`),
		"skill":     json.RawMessage(`{"advertise_tokens":2100,"loaded_skill_tokens":1500,"available_skill_tokens":600}`),
	}
	got := buildContextBudget(state, exts)
	if got == nil {
		t.Fatalf("buildContextBudget returned nil with every dimension populated")
	}
	if got.HistoryTokens != 1500 {
		t.Errorf("HistoryTokens = %d, want 1500", got.HistoryTokens)
	}
	if got.ToolsTokens != 2400 {
		t.Errorf("ToolsTokens = %d, want 2400", got.ToolsTokens)
	}
	if got.SessionUsage == nil ||
		got.SessionUsage.PromptTokens != 12000 ||
		got.SessionUsage.CompletionTokens != 3000 {
		t.Errorf("SessionUsage = %+v, want 12000→3000", got.SessionUsage)
	}
	if got.Extensions["compactor"] != 600 {
		t.Errorf("Extensions[compactor] = %d, want 600", got.Extensions["compactor"])
	}
	if got.Extensions["notepad"] != 400 {
		t.Errorf("Extensions[notepad] = %d, want 400", got.Extensions["notepad"])
	}
	if got.Extensions["plan"] != 200 {
		t.Errorf("Extensions[plan] = %d, want 200", got.Extensions["plan"])
	}
	// When the skill split (loaded_skill_tokens +
	// available_skill_tokens) is present, the legacy
	// advertise_tokens total is suppressed in Extensions so the
	// UI doesn't render the same number twice.
	if _, dup := got.Extensions["skill"]; dup {
		t.Errorf("Extensions[skill] = %d, want absent when split fields are set",
			got.Extensions["skill"])
	}
	if got.Skills == nil {
		t.Fatalf("Skills nil with split in skill payload")
	}
	if got.Skills.LoadedTokens != 1500 || got.Skills.AvailableTokens != 600 {
		t.Errorf("Skills = %+v, want loaded=1500 available=600", got.Skills)
	}
}

// TestBuildContextBudget_NilOnEmpty — pristine session emits
// no payload so the adapter skips rendering.
func TestBuildContextBudget_NilOnEmpty(t *testing.T) {
	state := &budgetTestState{id: "ses-empty"}
	if got := buildContextBudget(state, nil); got != nil {
		t.Fatalf("buildContextBudget on empty state = %+v, want nil", got)
	}
}

// TestBuildContextBudget_PartialPopulation — only some
// dimensions land. Empty maps + nil pointers don't appear in
// the payload (omitempty keeps the wire compact).
func TestBuildContextBudget_PartialPopulation(t *testing.T) {
	state := &budgetTestState{
		id:         "ses-partial",
		toolTokens: 500,
	}
	got := buildContextBudget(state, nil)
	if got == nil {
		t.Fatalf("expected partial budget with only tools_tokens")
	}
	if got.ToolsTokens != 500 {
		t.Errorf("ToolsTokens = %d, want 500", got.ToolsTokens)
	}
	if got.HistoryTokens != 0 {
		t.Errorf("HistoryTokens = %d, want 0", got.HistoryTokens)
	}
	if got.SessionUsage != nil {
		t.Errorf("SessionUsage = %+v, want nil", got.SessionUsage)
	}
	if got.Skills != nil {
		t.Errorf("Skills = %+v, want nil", got.Skills)
	}
	if len(got.Extensions) != 0 {
		t.Errorf("Extensions = %v, want empty", got.Extensions)
	}
}
