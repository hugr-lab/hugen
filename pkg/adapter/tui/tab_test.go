package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// TestModel_AttachTab_AddsTabAndSwitchesFocus covers the slice 4
// attach pipeline: an attachTabMsg appends a tab and the new tab
// becomes the active one.
func TestModel_AttachTab_AddsTabAndSwitchesFocus(t *testing.T) {
	m, _ := newTestModel(t)
	if len(m.tabs) != 1 {
		t.Fatalf("initial tabs = %d; want 1", len(m.tabs))
	}
	newT := newTab("sess-second", m.user, m.tabs[0].submit, m.logger)
	m2, _ := m.Update(attachTabMsg{t: newT})
	m = m2.(model)
	if len(m.tabs) != 2 {
		t.Fatalf("tabs after attach = %d; want 2", len(m.tabs))
	}
	if m.active != 1 {
		t.Errorf("active = %d; want 1 (focus jumps to new tab)", m.active)
	}
}

// TestModel_AttachTab_StartsPumpWithSub is the M1 regression
// guard: when attachTabMsg carries a non-nil subscription
// channel, the reducer invokes startPump exactly once with the
// new tab's session id. Combined with reading the post-update
// model below, this proves the contract — the tab is appended
// AND the pump is queued; the ordering (append-then-pump) is
// enforced by the reducer's source order at model.go's
// attachTabMsg branch.
func TestModel_AttachTab_StartsPumpWithSub(t *testing.T) {
	m, _ := newTestModel(t)
	var sids []string
	m.startPump = func(sid string, _ <-chan protocol.Frame) {
		sids = append(sids, sid)
	}
	sub := make(chan protocol.Frame, 1)
	newT := newTab("sess-pumped", m.user, m.tabs[0].submit, m.logger)
	m2, _ := m.Update(attachTabMsg{t: newT, sub: sub})
	m = m2.(model)
	if len(sids) != 1 || sids[0] != "sess-pumped" {
		t.Errorf("startPump should fire once for sess-pumped; got %v", sids)
	}
	if len(m.tabs) != 2 {
		t.Errorf("attach did not append; m.tabs len = %d", len(m.tabs))
	}
}

// TestModel_AttachTab_NilSubSkipsPump confirms backward-compat
// for tests that build attachTabMsg without a subscription
// channel — the reducer just appends and skips startPump.
func TestModel_AttachTab_NilSubSkipsPump(t *testing.T) {
	m, _ := newTestModel(t)
	var pumpCalled bool
	m.startPump = func(string, <-chan protocol.Frame) { pumpCalled = true }
	newT := newTab("sess-quiet", m.user, m.tabs[0].submit, m.logger)
	m.Update(attachTabMsg{t: newT}) // sub is nil
	if pumpCalled {
		t.Errorf("startPump must NOT be called when sub is nil")
	}
}

// TestModel_CycleTabs_CtrlRightCtrlLeftWrap exercises the canonical
// forward/back cycle bindings. ctrl+tab is not portable across
// terminals; ctrl+right/ctrl+left + ctrl+pgdown/ctrl+pgup are used
// instead (spec §8).
func TestModel_CycleTabs_CtrlRightCtrlLeftWrap(t *testing.T) {
	m, _ := newTestModel(t)
	// Add two more tabs so we have 0,1,2 (active=0 after init).
	for i := 0; i < 2; i++ {
		newT := newTab("sess-"+string(rune('A'+i)), m.user, m.tabs[0].submit, m.logger)
		m2, _ := m.Update(attachTabMsg{t: newT})
		m = m2.(model)
	}
	// Force focus back to tab 0 so the cycle starts there.
	m.active = 0

	// Forward: 0 → 1 → 2 → 0.
	for i, want := range []int{1, 2, 0} {
		m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlRight})
		m = m2.(model)
		if m.active != want {
			t.Fatalf("forward cycle step %d: active = %d; want %d", i, m.active, want)
		}
	}
	// Backward: 0 → 2 → 1 → 0.
	for i, want := range []int{2, 1, 0} {
		m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlLeft})
		m = m2.(model)
		if m.active != want {
			t.Fatalf("backward cycle step %d: active = %d; want %d", i, m.active, want)
		}
	}
}

// TestModel_FrameForNonActiveTab_FlipsDirty asserts the activity
// marker fires when a frame arrives on a non-focused tab.
func TestModel_FrameForNonActiveTab_FlipsDirty(t *testing.T) {
	m, _ := newTestModel(t)
	newT := newTab("sess-other", m.user, m.tabs[0].submit, m.logger)
	m2, _ := m.Update(attachTabMsg{t: newT})
	m = m2.(model)
	// Switch focus back to tab 0.
	m.active = 0

	// Send a frame addressed at the OTHER session.
	frame := &protocol.AgentMessage{
		BaseFrame: protocol.BaseFrame{Session: "sess-other"},
		Payload:   protocol.AgentMessagePayload{Text: "hi", Final: true, Consolidated: true},
	}
	m2, _ = m.Update(frameMsg{frame: frame})
	m = m2.(model)

	if !m.tabs[1].dirty {
		t.Errorf("non-active tab should be marked dirty on frame arrival")
	}
	if m.tabs[0].dirty {
		t.Errorf("active tab should NOT be marked dirty on its own frames")
	}
}

// TestModel_CycleClearsDirty asserts focusing a dirty tab clears
// the activity marker.
func TestModel_CycleClearsDirty(t *testing.T) {
	m, _ := newTestModel(t)
	newT := newTab("sess-other", m.user, m.tabs[0].submit, m.logger)
	m2, _ := m.Update(attachTabMsg{t: newT})
	m = m2.(model)
	m.active = 0
	m.tabs[1].dirty = true

	// Cycle forward → focus tab 1 → dirty cleared.
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlRight})
	m = m2.(model)
	if m.active != 1 {
		t.Fatalf("active after cycle = %d; want 1", m.active)
	}
	if m.tabs[1].dirty {
		t.Errorf("focused tab should clear dirty marker")
	}
}

// TestModel_View_RendersTabBar covers the top tab strip rendering
// — a 2-tab model puts both labels in the bar with the active one
// styled differently from the idle one.
func TestModel_View_RendersTabBar(t *testing.T) {
	m, _ := newTestModel(t)
	newT := newTab("sess-second", m.user, m.tabs[0].submit, m.logger)
	m2, _ := m.Update(attachTabMsg{t: newT})
	m = m2.(model)

	out := m.View()
	first := shortID(m.tabs[0].sessionID)
	second := shortID(m.tabs[1].sessionID)
	if !strings.Contains(out, first) {
		t.Errorf("tab bar missing first sessionID %q in view:\n%s", first, out)
	}
	if !strings.Contains(out, second) {
		t.Errorf("tab bar missing second sessionID %q in view:\n%s", second, out)
	}
}

// TestModel_CtrlN_WithoutOpenTabCallback_Recovers asserts the
// Ctrl+N path is safe when openTab is not installed (tests, narrow
// adapters). The active tab's banner flashes the failure but the
// model continues.
func TestModel_CtrlN_WithoutOpenTabCallback_Recovers(t *testing.T) {
	m, _ := newTestModel(t)
	m2, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlN})
	m = m2.(model)
	if cmd != nil {
		t.Errorf("Ctrl+N without callback should not produce a cmd; got %v", cmd)
	}
	if m.currentTab().bannerError == "" {
		t.Errorf("Ctrl+N without callback should flash a banner; got empty")
	}
}

// TestFormatTabCell_DirtyShowsAsterisk asserts the activity glyph
// renders for non-active dirty tabs.
func TestFormatTabCell_DirtyShowsAsterisk(t *testing.T) {
	t1 := newTab("sess-abc", protocol.ParticipantInfo{}, nil, nil)
	t1.dirty = true
	out := formatTabCell(t1, 0, false /*not active*/)
	if !strings.Contains(out, "*") {
		t.Errorf("dirty cell should contain '*'; got %q", out)
	}
	// Active cell suppresses the glyph (active is its own signal).
	t2 := newTab("sess-xyz", protocol.ParticipantInfo{}, nil, nil)
	t2.dirty = true
	out = formatTabCell(t2, 1, true /*active*/)
	if strings.Contains(out, "*") {
		t.Errorf("active cell should not show '*' even when dirty; got %q", out)
	}
}
