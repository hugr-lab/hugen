package tui

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

// rowFromFrame is a thin wrapper that turns a runtime Frame into
// the EventRow shape replay consumes. Tests use this to construct
// synthetic event logs without standing up a real DuckDB store.
func rowFromFrame(t *testing.T, seq int, f protocol.Frame) store.EventRow {
	t.Helper()
	row, _, err := store.FrameToEventRow(f, "agent-test")
	if err != nil {
		t.Fatalf("FrameToEventRow(%T): %v", f, err)
	}
	row.Seq = seq
	return row
}

func TestReplayEvents_ProjectsUserAndAssistantIntoChat(t *testing.T) {
	user := protocol.ParticipantInfo{ID: "u", Kind: protocol.ParticipantUser, Name: "u"}
	agent := protocol.ParticipantInfo{ID: "a", Kind: protocol.ParticipantAgent, Name: "hugen"}
	tb := newTab("ses-replay", user, func(protocol.Frame) error { return nil }, slog.Default())
	tb.applyGeometry(80, 20, 100)

	rows := []store.EventRow{
		rowFromFrame(t, 1, protocol.NewUserMessage("ses-replay", user, "hello")),
		rowFromFrame(t, 2, protocol.NewAgentMessageConsolidated(
			"ses-replay", agent, "Hi there!", 0, true, nil, "", "")),
	}
	replayEvents(tb, rows)

	// The chat buffer should hold one user span + one assistant span.
	gotKinds := make([]chatSpanKind, 0, len(tb.chat.spans))
	for _, s := range tb.chat.spans {
		gotKinds = append(gotKinds, s.kind)
	}
	if len(gotKinds) < 2 {
		t.Fatalf("expected ≥ 2 spans after replay; got %d (%+v)", len(gotKinds), tb.chat.spans)
	}
	// First span must be the user message.
	if tb.chat.spans[0].kind != spanUser || !strings.Contains(tb.chat.spans[0].text, "hello") {
		t.Errorf("first span wrong: %+v", tb.chat.spans[0])
	}
	// Some assistant span must carry the reply text.
	var found bool
	for _, s := range tb.chat.spans {
		if s.kind == spanAssistant && strings.Contains(s.text, "Hi there!") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("missing assistant span with reply text in %+v", tb.chat.spans)
	}
	if tb.statusLine != "ready" {
		t.Errorf("statusLine after replay = %q; want ready", tb.statusLine)
	}
}

// TestReplayEvents_SkipsAgentAuthoredUserMessages — synthetic
// UserMessages authored by the agent participant (e.g. the
// [system:async_summary] kick from kickAsyncSummaryTurn) must not
// render as user bubbles during resume. Live mode hides them via
// the UserMessage early-return in handleFrame; replay needs the
// same discipline. Phase 5.x.skill-polish-1 dogfood fix.
func TestReplayEvents_SkipsAgentAuthoredUserMessages(t *testing.T) {
	user := protocol.ParticipantInfo{ID: "u", Kind: protocol.ParticipantUser, Name: "u"}
	// Author.ID == agent_id ("agent-test" — see rowFromFrame helper)
	// so the store's deriveAuthorKind on replay returns ParticipantAgent.
	// Mirrors production where agent.Participant().ID == agent.ID().
	agent := protocol.ParticipantInfo{ID: "agent-test", Kind: protocol.ParticipantAgent, Name: "hugen"}
	tb := newTab("ses-async", user, func(protocol.Frame) error { return nil }, slog.Default())
	tb.applyGeometry(80, 20, 100)

	rows := []store.EventRow{
		rowFromFrame(t, 1, protocol.NewUserMessage("ses-async", user, "real user msg")),
		rowFromFrame(t, 2, protocol.NewUserMessage("ses-async", agent, "[system:async_summary] kick text")),
		rowFromFrame(t, 3, protocol.NewAgentMessageConsolidated(
			"ses-async", agent, "Mission completed: …", 0, true, nil, "", "")),
	}
	replayEvents(tb, rows)

	userSpans := 0
	for _, s := range tb.chat.spans {
		if s.kind == spanUser {
			userSpans++
			if strings.Contains(s.text, "[system:async_summary]") {
				t.Errorf("synthetic kick rendered as user span: %q", s.text)
			}
		}
	}
	if userSpans != 1 {
		t.Errorf("user spans = %d; want 1 (real user msg only)", userSpans)
	}
}

func TestReplayEvents_SkipsInflightAndCleanState(t *testing.T) {
	user := protocol.ParticipantInfo{ID: "u", Kind: protocol.ParticipantUser, Name: "u"}
	agent := protocol.ParticipantInfo{ID: "a", Kind: protocol.ParticipantAgent, Name: "hugen"}
	tb := newTab("ses-mid", user, func(protocol.Frame) error { return nil }, slog.Default())
	tb.applyGeometry(80, 20, 100)

	// Pre-consolidation streaming chunk (Final=false / Consolidated=false).
	// Replay skips these — the consolidated row is the durable record.
	chunk := protocol.NewAgentMessage("ses-mid", agent, "half-typed", 0, false)
	row := rowFromFrame(t, 1, chunk)
	replayEvents(tb, []store.EventRow{row})

	if tb.chat.pendingAssistant.Len() != 0 {
		t.Errorf("pendingAssistant should be empty after replay; got %q", tb.chat.pendingAssistant.String())
	}
	for _, s := range tb.chat.spans {
		if s.kind == spanAssistant {
			t.Errorf("non-consolidated chunks must NOT produce assistant spans; got %+v", s)
		}
	}
}

func TestReplayEvents_SkipsUndecodableRows(t *testing.T) {
	user := protocol.ParticipantInfo{ID: "u", Kind: protocol.ParticipantUser, Name: "u"}
	tb := newTab("ses-skip", user, func(protocol.Frame) error { return nil }, slog.Default())
	tb.applyGeometry(80, 20, 100)

	// Row with an unknown EventType — EventRowToFrame currently
	// returns an OpaqueFrame for these (won't error). To produce a
	// hard decode failure we ship a row whose payload contradicts
	// the kind (e.g. tool_call without tool_name).
	bogus := store.EventRow{
		ID:        "bad",
		SessionID: "ses-skip",
		Seq:       1,
		EventType: "tool_call",
		// Missing ToolName / ToolArgs — FrameToEventRow inverse
		// would fail on decoding the Frame variant. If the
		// inverse decodes anyway, this test simply asserts the
		// "no panic" property: replay must not crash on weird
		// rows.
	}
	good := rowFromFrame(t, 2, protocol.NewUserMessage("ses-skip", user, "real"))
	replayEvents(tb, []store.EventRow{bogus, good})

	// The "real" user message must still land.
	var found bool
	for _, s := range tb.chat.spans {
		if s.kind == spanUser && strings.Contains(s.text, "real") {
			found = true
		}
	}
	if !found {
		t.Errorf("good row was not projected after a malformed predecessor: %+v", tb.chat.spans)
	}
}

func TestModel_CloseTab_InvokesForgetCallback(t *testing.T) {
	m, _ := newTestModel(t)
	var forgotten []string
	m.forgetTab = func(id string) { forgotten = append(forgotten, id) }
	// Add a second tab so close doesn't tea.Quit.
	m.tabs = append(m.tabs, newTab("ses-extra", m.user, m.tabs[0].submit, m.logger))
	_ = m.closeTab(1)
	if len(forgotten) != 1 || forgotten[0] != "ses-extra" {
		t.Errorf("forgetTab not called for closed id; got %v", forgotten)
	}
}
