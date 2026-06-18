package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// TestModel_BatchedOptionalClarification_HighlightCommits is the
// regression for the empty-answer bug: a batched OPTIONAL clarification
// whose option the operator picked via ↑↓ highlight + Enter used to
// submit an empty Answers map (the highlight→value fallback was gated to
// `required` kind only). The runtime then saw no answer and the model
// improvised. The operator's pick must be captured for every kind.
func TestModel_BatchedOptionalClarification_HighlightCommits(t *testing.T) {
	m, submitted := newTestModel(t)
	req := &protocol.InquiryRequest{
		BaseFrame: protocol.BaseFrame{Session: "sess-opt12345"},
		Payload: protocol.InquiryRequestPayload{
			RequestID:       "req-opt",
			CallerSessionID: "worker-9",
			Type:            protocol.InquiryTypeClarification,
			Clarifications: []protocol.Clarification{{
				ID:       "type",
				Kind:     protocol.ClarificationKindOptional,
				Question: "Which geozone type?",
				Options:  []string{"88 — urban/rural", "86 — districts", "both"},
			}},
		},
	}
	m2, _ := m.Update(frameMsg{frame: req})
	m = m2.(model)

	// ↓ highlights the first option, Enter commits + advances to the
	// review screen, Enter again submits the batch.
	for _, k := range []tea.KeyMsg{
		{Type: tea.KeyDown},
		{Type: tea.KeyEnter},
		{Type: tea.KeyEnter},
	} {
		m2, _ = m.Update(k)
		m = m2.(model)
	}

	f := submitted.Load()
	if f == nil {
		t.Fatal("nothing submitted")
	}
	resp, ok := (*f).(*protocol.InquiryResponse)
	if !ok {
		t.Fatalf("submitted frame is %T, want *InquiryResponse", *f)
	}
	got := resp.Payload.Answers["type"].Value
	if got != "88 — urban/rural" {
		t.Fatalf("optional highlighted option not captured: Answers[type].Value=%q, want the first option (empty-answer regression)", got)
	}
}
