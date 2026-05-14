package tui

import (
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

func newTestModel(t *testing.T) (model, *atomic.Pointer[protocol.Frame]) {
	t.Helper()
	var lastSubmitted atomic.Pointer[protocol.Frame]
	submit := func(f protocol.Frame) error {
		lastSubmitted.Store(&f)
		return nil
	}
	user := protocol.ParticipantInfo{ID: "tester", Kind: protocol.ParticipantUser, Name: "tester"}
	m := newModel("sess-abc12345", user, submit, slog.Default())
	// Simulate the first WindowSizeMsg so relayout populates geometry.
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	return updated.(model), &lastSubmitted
}

func TestModel_StreamingAgentMessage_FinalizesIntoChat(t *testing.T) {
	m, _ := newTestModel(t)

	// Chunked, non-final assistant frames.
	for _, chunk := range []string{"Hel", "lo, ", "world!"} {
		m2, _ := m.Update(frameMsg{frame: &protocol.AgentMessage{
			Payload: protocol.AgentMessagePayload{
				Text:         chunk,
				Final:        false,
				Consolidated: false,
			},
		}})
		m = m2.(model)
	}
	if got := m.currentTab().chat.pendingAssistant.String(); got != "Hello, world!" {
		t.Fatalf("pendingAssistant accumulator = %q; want %q", got, "Hello, world!")
	}

	// Final consolidated → flushes the accumulator into spans.
	m2, _ := m.Update(frameMsg{frame: &protocol.AgentMessage{
		Payload: protocol.AgentMessagePayload{
			Text:         "Hello, world!",
			Final:        true,
			Consolidated: true,
		},
	}})
	m = m2.(model)
	if m.currentTab().chat.pendingAssistant.Len() != 0 {
		t.Fatalf("pendingAssistant not reset after final-consolidated frame")
	}
	if len(m.currentTab().chat.spans) != 1 || m.currentTab().chat.spans[0].kind != spanAssistant {
		t.Fatalf("expected one assistant span, got %d spans", len(m.currentTab().chat.spans))
	}
	if !strings.Contains(m.currentTab().chat.spans[0].text, "Hello, world!") {
		t.Fatalf("assistant span text = %q; missing payload", m.currentTab().chat.spans[0].text)
	}
	if m.currentTab().statusLine != "ready" {
		t.Fatalf("statusLine = %q; want ready after final consolidated", m.currentTab().statusLine)
	}
}

func TestModel_ErrorFrame_SetsBanner(t *testing.T) {
	m, _ := newTestModel(t)
	m2, _ := m.Update(frameMsg{frame: &protocol.Error{
		Payload: protocol.ErrorPayload{Code: "boom", Message: "something failed"},
	}})
	m = m2.(model)
	if !strings.Contains(m.currentTab().bannerError, "boom") || !strings.Contains(m.currentTab().bannerError, "something failed") {
		t.Fatalf("bannerError = %q; want code+message", m.currentTab().bannerError)
	}
}

func TestModel_CtrlC_SubmitsEndAndEntersClosing(t *testing.T) {
	m, submitted := newTestModel(t)
	m2, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	m = m2.(model)
	if !m.currentTab().closing {
		t.Fatalf("Ctrl+C must transition model into closing state")
	}
	if cmd != nil {
		// Ctrl+C path should return nil cmd; tea.Quit only fires on
		// SessionClosed echo (see TestModel_SessionClosed_TriggersQuit).
		t.Fatalf("Ctrl+C returned a tea.Cmd; expected nil — quit waits for SessionClosed")
	}
	got := submitted.Load()
	if got == nil {
		t.Fatalf("Ctrl+C did not submit any frame")
	}
	sc, ok := (*got).(*protocol.SlashCommand)
	if !ok {
		t.Fatalf("submitted frame = %T; want *protocol.SlashCommand", *got)
	}
	if sc.Payload.Name != "end" {
		t.Fatalf("submitted slash command name = %q; want end", sc.Payload.Name)
	}
}

func TestModel_SessionClosed_TriggersQuit_OnlyWhenClosing(t *testing.T) {
	m, _ := newTestModel(t)

	// Unsolicited SessionClosed (no prior Ctrl+C) — must NOT quit.
	m2, cmd := m.Update(frameMsg{frame: &protocol.SessionClosed{
		Payload: protocol.SessionClosedPayload{Reason: "remote"},
	}})
	m = m2.(model)
	if cmd != nil {
		t.Fatalf("SessionClosed without prior close-intent issued a tea.Cmd; expected nil")
	}

	// Now user initiates close — Ctrl+C — then SessionClosed quits.
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	m = m2.(model)
	m2, cmd = m.Update(frameMsg{frame: &protocol.SessionClosed{
		Payload: protocol.SessionClosedPayload{Reason: "user"},
	}})
	m = m2.(model)
	if cmd == nil {
		t.Fatalf("SessionClosed after closing-intent must return tea.Quit cmd")
	}
}

func TestModel_EnterSubmitsUserMessage(t *testing.T) {
	m, submitted := newTestModel(t)
	m.currentTab().textarea.SetValue("hello")
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = m2.(model)
	if m.currentTab().textarea.Value() != "" {
		t.Fatalf("textarea not reset after Enter")
	}
	got := submitted.Load()
	if got == nil {
		t.Fatalf("Enter did not submit anything")
	}
	um, ok := (*got).(*protocol.UserMessage)
	if !ok {
		t.Fatalf("submitted frame = %T; want *protocol.UserMessage", *got)
	}
	if um.Payload.Text != "hello" {
		t.Fatalf("user message text = %q; want hello", um.Payload.Text)
	}
	// User bubble must appear in the chat buffer immediately.
	if len(m.currentTab().chat.spans) == 0 || m.currentTab().chat.spans[0].kind != spanUser {
		t.Fatalf("expected user span echoed in chat; got %d spans", len(m.currentTab().chat.spans))
	}
}

func TestModel_SlashCommandRoutesToSlashFrame(t *testing.T) {
	m, submitted := newTestModel(t)
	m.currentTab().textarea.SetValue("/end")
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	_ = m2
	got := submitted.Load()
	if got == nil {
		t.Fatalf("Enter on /end did not submit")
	}
	sc, ok := (*got).(*protocol.SlashCommand)
	if !ok {
		t.Fatalf("submitted frame = %T; want *protocol.SlashCommand", *got)
	}
	if sc.Payload.Name != "end" {
		t.Fatalf("slash name = %q; want end", sc.Payload.Name)
	}
}

func TestModel_View_BeforeReadyShowsBootBanner(t *testing.T) {
	user := protocol.ParticipantInfo{ID: "tester", Kind: protocol.ParticipantUser, Name: "tester"}
	m := newModel("sess-x", user, func(protocol.Frame) error { return nil }, slog.Default())
	out := m.View()
	if !strings.Contains(out, "starting") {
		t.Fatalf("pre-ready View() = %q; want boot banner", out)
	}
}

func TestModel_ReasoningAutoFlushOnNonReasoningFrame(t *testing.T) {
	m, _ := newTestModel(t)
	// Stream a few reasoning chunks WITHOUT a final flag — mimics
	// model adapters that never close out reasoning explicitly.
	for _, chunk := range []string{"first ", "thought ", "in progress"} {
		m2, _ := m.Update(frameMsg{frame: &protocol.Reasoning{
			Payload: protocol.ReasoningPayload{Text: chunk, Final: false},
		}})
		m = m2.(model)
	}
	if m.currentTab().chat.pendingReasoning.Len() == 0 {
		t.Fatalf("expected pendingReasoning to accumulate")
	}
	// Any non-reasoning frame must auto-flush the accumulator into
	// a finalized span so subsequent turns do not inherit it.
	m2, _ := m.Update(frameMsg{frame: &protocol.ToolCall{
		Payload: protocol.ToolCallPayload{Name: "demo.tool"},
	}})
	m = m2.(model)
	if m.currentTab().chat.pendingReasoning.Len() != 0 {
		t.Fatalf("pendingReasoning still set after non-reasoning frame")
	}
	if got := lastSpanKind(m.currentTab().chat); got != spanReasoning {
		t.Fatalf("expected finalized reasoning span; last kind = %v", got)
	}
}

func TestModel_UserSubmitFlushesStalePendingReasoning(t *testing.T) {
	m, _ := newTestModel(t)
	// Stuck reasoning from a previous turn.
	m.currentTab().chat.appendReasoningChunk("stale from prior turn")

	m.currentTab().textarea.SetValue("next prompt")
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = m2.(model)
	if m.currentTab().chat.pendingReasoning.Len() != 0 {
		t.Fatalf("user submit did not flush stale pendingReasoning")
	}
	// The flushed reasoning becomes a span ABOVE the new user bubble.
	if len(m.currentTab().chat.spans) < 2 {
		t.Fatalf("expected at least 2 spans (flushed reasoning + user); got %d", len(m.currentTab().chat.spans))
	}
	if m.currentTab().chat.spans[0].kind != spanReasoning {
		t.Fatalf("first span kind = %v; want spanReasoning (flushed before user echo)", m.currentTab().chat.spans[0].kind)
	}
	if m.currentTab().chat.spans[1].kind != spanUser {
		t.Fatalf("second span kind = %v; want spanUser", m.currentTab().chat.spans[1].kind)
	}
}

func lastSpanKind(c *chatBuffer) chatSpanKind {
	if len(c.spans) == 0 {
		return -1
	}
	return c.spans[len(c.spans)-1].kind
}
