package tui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

func TestParseLiveviewStatus_FullPayload(t *testing.T) {
	now := time.Now().UTC()
	payload := map[string]any{
		"session_id":      "ses-root",
		"depth":           0,
		"lifecycle_state": "wait_subagents",
		"last_tool_call":  protocol.ToolCallRef{Name: "hugr.execute_query", StartedAt: now.Add(-3 * time.Second)},
		"extensions": map[string]any{
			"skill":   map[string]any{"loaded": []string{"_root", "analyst"}},
			"plan":    map[string]any{"Active": true, "Text": "Investigate data sources", "CurrentStep": "Explore", "Comments": []any{1, 2}},
			"notepad": map[string]any{
				"recent": []map[string]any{{"id": "n1", "category": "schema-finding"}, {"id": "n2", "category": "schema-finding"}, {"id": "n3", "category": "chat-answer"}},
				"counts": map[string]int{"schema-finding": 2, "chat-answer": 1},
			},
		},
		"children": map[string]any{
			"ses-mission": map[string]any{
				"session_id":      "ses-mission",
				"depth":           1,
				"lifecycle_state": "active",
				"last_tool_call":  protocol.ToolCallRef{Name: "analyst.spawn", StartedAt: now.Add(-10 * time.Second)},
			},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s, err := parseLiveviewStatus(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if s.SessionID != "ses-root" {
		t.Errorf("SessionID = %q", s.SessionID)
	}
	if s.LifecycleState != "wait_subagents" {
		t.Errorf("LifecycleState = %q", s.LifecycleState)
	}
	if s.LastToolCall == nil || s.LastToolCall.Name != "hugr.execute_query" {
		t.Errorf("LastToolCall = %+v", s.LastToolCall)
	}
	if len(s.Children) != 1 {
		t.Errorf("Children = %d; want 1", len(s.Children))
	}
	child := s.Children["ses-mission"]
	if child == nil || child.Depth != 1 || child.LifecycleState != "active" {
		t.Errorf("child = %+v", child)
	}
}

func TestRenderSidebar_NilStatusShowsPlaceholder(t *testing.T) {
	out := renderSidebar(nil, 36)
	if !strings.Contains(out, "waiting") {
		t.Fatalf("nil status sidebar = %q; want placeholder", out)
	}
}

func TestRenderSidebar_HappyPathContainsExpectedSections(t *testing.T) {
	s := &liveviewStatus{
		SessionID:      "ses-root",
		Depth:          0,
		LifecycleState: "wait_subagents",
		LastToolCall:   &protocol.ToolCallRef{Name: "hugr.execute_query", StartedAt: time.Now().Add(-5 * time.Second)},
		Extensions: map[string]json.RawMessage{
			"skill":   json.RawMessage(`{"loaded":["_root","analyst"],"tools":12}`),
			"plan":    json.RawMessage(`{"Active":true,"Text":"Investigate","CurrentStep":"Explore","Comments":[{},{}]}`),
			"notepad": json.RawMessage(`{"recent":[{"id":"n1","category":"schema-finding"}],"counts":{"schema-finding":12,"chat-answer":1}}`),
		},
		Children: map[string]*liveviewStatus{
			"ses-m": {SessionID: "ses-m", Depth: 1, LifecycleState: "active", LastToolCall: &protocol.ToolCallRef{Name: "analyst.spawn"}},
		},
	}
	out := renderSidebar(s, 36)
	for _, want := range []string{
		"Tier: root", "wait_subagents", "Subagents", "mission",
		"Last tool", "hugr.execute_query", "Plan", "Explore",
		"Notepad", "schema-finding",
		"Skills", "_root", "analyst", "12 tools",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("sidebar missing %q in:\n%s", want, out)
		}
	}
}

func TestRenderSidebar_PendingInquiryProminent(t *testing.T) {
	s := &liveviewStatus{
		SessionID:      "ses-root",
		Depth:          0,
		LifecycleState: "wait_approval",
		PendingInquiry: &protocol.PendingInquiryRef{
			RequestID: "req-1",
			Type:      "approval",
			Question:  "Run bash.shell?",
			StartedAt: time.Now(),
		},
	}
	out := renderSidebar(s, 36)
	if !strings.Contains(out, "inquiry pending") {
		t.Fatalf("missing inquiry banner: %s", out)
	}
	if !strings.Contains(out, "Run bash.shell?") {
		t.Fatalf("missing inquiry question text: %s", out)
	}
}

// TestRenderSubagent_ShowsRoleFromChildMeta — dogfood feedback:
// the operator wants to see the spawn role on each subagent node
// (e.g. "mission:schema-explorer") rather than just the tier
// label.
func TestRenderSubagent_ShowsRoleFromChildMeta(t *testing.T) {
	worker := &liveviewStatus{SessionID: "w", Depth: 2, LifecycleState: "active"}
	out := renderSubagent(worker, childMetaEntry{Role: "schema-explorer", Skill: "analyst"}, 1, 60)
	if !strings.Contains(out, "worker:schema-explorer") {
		t.Errorf("missing role-augmented label: %s", out)
	}
}

func TestFormatNotifySubagent_FollowUpMarker(t *testing.T) {
	got := formatNotifySubagent(map[string]any{
		"subagent_id": "ses-abcdef1234",
		"content":     "also add a per-company table",
	})
	if !strings.Contains(got, "📨") {
		t.Errorf("missing icon: %q", got)
	}
	if !strings.Contains(got, "ses-abcd") { // shortID truncation
		t.Errorf("missing target: %q", got)
	}
	if !strings.Contains(got, "also add a per-company table") {
		t.Errorf("missing content preview: %q", got)
	}
}

func TestFormatSpawnMission_ShowsSkillAndGoal(t *testing.T) {
	got := formatSpawnMission(map[string]any{
		"skill": "analyst",
		"goal":  "count providers in op2023",
		"wait":  "async",
	})
	for _, want := range []string{"🚀", "analyst", "async", "count providers"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in spawn marker: %q", want, got)
		}
	}
}

func TestFormatAsyncMissionCompleted_HappyPath(t *testing.T) {
	p := &protocol.SubagentResultPayload{
		SessionID: "ses-abcdef1234",
		Reason:    protocol.TerminationCompleted,
		Result:    "Top providers: ACME, Globex, Initech.",
		Goal:      "count providers",
	}
	got := formatAsyncMissionCompleted(p)
	for _, want := range []string{"✓", "ses-abcd", "Top providers"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in async-completed marker: %q", want, got)
		}
	}
}

func TestFormatAsyncMissionCompleted_FallsBackToGoal(t *testing.T) {
	p := &protocol.SubagentResultPayload{
		SessionID: "ses-deadbeef",
		Reason:    protocol.TerminationCompleted,
		Goal:      "enumerate northwind",
	}
	got := formatAsyncMissionCompleted(p)
	if !strings.Contains(got, "enumerate northwind") {
		t.Errorf("expected goal fallback when Result empty: %q", got)
	}
}

func TestFormatAsyncMissionCompleted_AbnormalBadges(t *testing.T) {
	cancelled := formatAsyncMissionCompleted(&protocol.SubagentResultPayload{
		SessionID: "ses-x",
		Reason:    "subagent_cancel: user request",
		Result:    "stopped",
	})
	if !strings.Contains(cancelled, "⊘") {
		t.Errorf("expected ⊘ for cancel; got %q", cancelled)
	}
	cascade := formatAsyncMissionCompleted(&protocol.SubagentResultPayload{
		SessionID: "ses-y",
		Reason:    protocol.TerminationCancelCascade,
	})
	if !strings.Contains(cascade, "⊘") {
		t.Errorf("expected ⊘ for cancel_cascade; got %q", cascade)
	}
	errored := formatAsyncMissionCompleted(&protocol.SubagentResultPayload{
		SessionID: "ses-z",
		Reason:    "error: io",
		Result:    "boom",
	})
	if !strings.Contains(errored, "✗") {
		t.Errorf("expected ✗ for error reason; got %q", errored)
	}
	userCancel := formatAsyncMissionCompleted(&protocol.SubagentResultPayload{
		SessionID: "ses-u",
		Reason:    protocol.TerminationUserCancelPrefix + "/mission",
		Result:    "halted",
	})
	if !strings.Contains(userCancel, "⊘") {
		t.Errorf("expected ⊘ for user_cancel; got %q", userCancel)
	}
}

func TestRenderRecentChild_ShowsRole(t *testing.T) {
	rc := recentChildEntry{
		SessionID:    "ses-x",
		Depth:        2,
		Role:         "query-builder",
		Reason:       "completed",
		TerminatedAt: time.Now().Add(-5 * time.Second),
	}
	out := renderRecentChild(rc, 60)
	if !strings.Contains(out, "worker:query-builder") {
		t.Errorf("missing role on recent child: %s", out)
	}
}

func TestRenderSubagent_RecursiveDepth(t *testing.T) {
	worker := &liveviewStatus{SessionID: "w", Depth: 2, LifecycleState: "active",
		LastToolCall: &protocol.ToolCallRef{Name: "hugr.execute_query"}}
	mission := &liveviewStatus{SessionID: "m", Depth: 1, LifecycleState: "wait_subagents",
		Children: map[string]*liveviewStatus{"w": worker}}
	out := renderSubagent(mission, childMetaEntry{}, 1, 60)
	if !strings.Contains(out, "▸ mission") {
		t.Errorf("missing mission node: %s", out)
	}
	if !strings.Contains(out, "  ▸ worker") {
		t.Errorf("missing nested worker node: %s", out)
	}
	if !strings.Contains(out, "hugr.execute_query") {
		t.Errorf("missing worker's last_tool_call: %s", out)
	}
}

// TestRenderSubagent_RecentActivityPrintsHistory — phase 5.1c S1.
// When the subagent's projection carries recent_activity, render
// the last 2-3 tools as a stripe (most-recent first) instead of
// the single last_tool_call.
// TestRenderSidebar_RecentChildrenSection covers the dogfood
// follow-up: when a wave rolls over, completed children appear in
// the Recent section with reason + last_tool so the operator can
// see "what just finished" without scrolling the event log.
func TestRenderSidebar_RecentChildrenSection(t *testing.T) {
	s := &liveviewStatus{
		SessionID:      "ses-root",
		Depth:          0,
		LifecycleState: "active",
		RecentChildren: []recentChildEntry{
			{SessionID: "ses-a", Depth: 1, Reason: "completed",
				LastTool: "hugr.execute_query", TerminatedAt: time.Now().Add(-5 * time.Second)},
			{SessionID: "ses-b", Depth: 2, Reason: "error: stream_error",
				LastTool: "duckdb.exec_sql", TerminatedAt: time.Now().Add(-30 * time.Second)},
			{SessionID: "ses-c", Depth: 1, Reason: "cancel_cascade",
				TerminatedAt: time.Now().Add(-1 * time.Minute)},
		},
	}
	out := renderSidebar(s, 60)
	for _, want := range []string{
		"Recent",
		"mission",       // depth=1 entries
		"worker",        // depth=2 entries
		"completed",
		"stream_error",  // error reason short form
		"cancel_cascade",
		"hugr.execute_query",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Recent section missing %q in:\n%s", want, out)
		}
	}
}

func TestReasonShort_TrimsErrorPrefix(t *testing.T) {
	if got := reasonShort("error: stream_error"); got != "stream_error" {
		t.Errorf("reasonShort = %q; want stream_error", got)
	}
	if got := reasonShort("completed"); got != "completed" {
		t.Errorf("reasonShort = %q; want completed", got)
	}
}

func TestRenderSubagent_RecentActivityPrintsHistory(t *testing.T) {
	now := time.Now()
	worker := &liveviewStatus{
		SessionID:      "w",
		Depth:          2,
		LifecycleState: "active",
		RecentActivity: []protocol.ToolCallRef{
			{Name: "hugr.execute_query", StartedAt: now.Add(-2 * time.Second)},
			{Name: "duckdb.exec_sql", StartedAt: now.Add(-10 * time.Second)},
			{Name: "notepad.append", StartedAt: now.Add(-30 * time.Second)},
		},
	}
	out := renderSubagent(worker, childMetaEntry{}, 1, 60)
	for _, name := range []string{"hugr.execute_query", "duckdb.exec_sql", "notepad.append"} {
		if !strings.Contains(out, name) {
			t.Errorf("missing %q in subagent activity stripe:\n%s", name, out)
		}
	}
}

func TestParseNotepadCounts_GroupsAndSorts(t *testing.T) {
	// counts is server-side aggregate — the sidebar trusts it
	// verbatim and does NOT re-derive from `recent`.
	exts := map[string]json.RawMessage{
		"notepad": json.RawMessage(`{
			"recent": [{"id":"n1","category":"schema-finding"}],
			"counts": {"schema-finding": 17, "chat-answer": 5, "plan-step": 1}
		}`),
	}
	got := parseNotepadCounts(exts)
	if len(got) != 3 {
		t.Fatalf("buckets = %d; want 3", len(got))
	}
	if got[0].Category != "schema-finding" || got[0].Count != 17 {
		t.Errorf("top = %+v", got[0])
	}
	if got[1].Category != "chat-answer" || got[1].Count != 5 {
		t.Errorf("second = %+v", got[1])
	}
	if got[2].Category != "plan-step" || got[2].Count != 1 {
		t.Errorf("third = %+v", got[2])
	}
}

func TestParseNotepadCounts_EmptyCategoryLabelled(t *testing.T) {
	exts := map[string]json.RawMessage{
		"notepad": json.RawMessage(`{"counts":{"":4,"schema-finding":2}}`),
	}
	got := parseNotepadCounts(exts)
	if len(got) != 2 {
		t.Fatalf("buckets = %d; want 2", len(got))
	}
	// Most-numerous (empty key, surfaced as "uncategorized") first.
	if got[0].Category != "uncategorized" || got[0].Count != 4 {
		t.Errorf("top = %+v; want uncategorized=4", got[0])
	}
}

func TestParsePlan_InactivePlanReturnsZeroValue(t *testing.T) {
	exts := map[string]json.RawMessage{
		"plan": json.RawMessage(`{"Active":false}`),
	}
	p := parsePlan(exts)
	if p == nil {
		t.Fatalf("parsePlan returned nil for inactive plan; want zero-value snapshot")
	}
	if p.Active {
		t.Errorf("Active = true; want false")
	}
}

func TestParseLiveviewStatus_MalformedJSONReturnsError(t *testing.T) {
	if _, err := parseLiveviewStatus(json.RawMessage(`{"depth":"not-a-number"}`)); err == nil {
		t.Fatalf("expected error on malformed payload")
	}
}

func TestTruncate_HandlesNarrowWidth(t *testing.T) {
	cases := []struct {
		in    string
		width int
		want  string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello", 4, "hel…"},
		{"hello", 1, "…"},
		{"hello", 0, "hello"}, // 0 means "no limit"
	}
	for _, c := range cases {
		if got := truncate(c.in, c.width); got != c.want {
			t.Errorf("truncate(%q,%d) = %q; want %q", c.in, c.width, got, c.want)
		}
	}
}

func TestModel_ExtensionFrame_LiveviewStatusPopulatesSidebar(t *testing.T) {
	m, _ := newTestModel(t)
	payload := map[string]any{
		"session_id":      "ses-root",
		"depth":           0,
		"lifecycle_state": "active",
	}
	data, _ := json.Marshal(payload)
	frame := protocol.NewExtensionFrame(
		"ses-root", protocol.ParticipantInfo{},
		"liveview", protocol.CategoryMarker, "status", data,
	)
	m2, _ := m.Update(frameMsg{frame: frame})
	m = m2.(model)
	if m.currentTab().sidebarStatus == nil {
		t.Fatalf("liveview/status frame did not populate sidebarStatus")
	}
	if m.currentTab().sidebarStatus.LifecycleState != "active" {
		t.Errorf("LifecycleState = %q", m.currentTab().sidebarStatus.LifecycleState)
	}
}

func TestModel_ExtensionFrame_NonLiveviewIgnoredBySidebar(t *testing.T) {
	m, _ := newTestModel(t)
	frame := protocol.NewExtensionFrame(
		"ses-root", protocol.ParticipantInfo{},
		"plan", protocol.CategoryMarker, "set", json.RawMessage(`{"Active":true}`),
	)
	m2, _ := m.Update(frameMsg{frame: frame})
	m = m2.(model)
	if m.currentTab().sidebarStatus != nil {
		t.Fatalf("non-liveview extension frame mutated sidebarStatus")
	}
}

func TestRelayout_HidesSidebarOnNarrowTerminal(t *testing.T) {
	m, _ := newTestModel(t)
	// Force a narrow window.
	m2, _ := m.Update(struct{ Width, Height int }{}) // no-op
	_ = m2
	// Simulate WindowSizeMsg via direct relayout.
	m.width, m.height = 70, 30
	m.relayout()
	if m.sidebarShown {
		t.Fatalf("sidebar should hide at 70-col terminal")
	}
	m.width = 100
	m.relayout()
	if !m.sidebarShown {
		t.Fatalf("sidebar should show at 100-col terminal")
	}
}
