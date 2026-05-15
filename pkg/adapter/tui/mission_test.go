package tui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// fakeLiveviewStatus builds a liveviewStatus with `n` direct
// children. Each child gets a synthetic 8-char hex id, a depth of
// 1 (mission tier), and a childMeta entry naming the role.
func fakeLiveviewStatus(n int) *liveviewStatus {
	s := &liveviewStatus{
		SessionID: "root-aaaaaa01",
		Depth:     0,
		Children:  make(map[string]*liveviewStatus, n),
		ChildMeta: make(map[string]childMetaEntry, n),
	}
	roles := []string{"data-analyst", "schema-explorer", "report-builder", "query-builder", "general"}
	for i := 0; i < n; i++ {
		id := "child" + string(rune('A'+i)) + "0000001"
		s.Children[id] = &liveviewStatus{SessionID: id, Depth: 1}
		s.ChildMeta[id] = childMetaEntry{
			Role:      roles[i%len(roles)],
			Skill:     "analyst",
			Task:      "goal " + string(rune('A'+i)),
			StartedAt: time.Now().Add(-time.Duration(i+1) * time.Minute),
		}
	}
	return s
}

// TestRenderMissionModal_EmptyList — modal renders an explicit
// "no missions running" body and the dismiss hint when the
// child list is empty.
func TestRenderMissionModal_EmptyList(t *testing.T) {
	state := newMissionModalState(nil)
	out := renderMissionModal(state, 60)
	if !strings.Contains(out, "No missions running.") {
		t.Errorf("missing empty-list message; got:\n%s", out)
	}
	if !strings.Contains(out, "esc") {
		t.Errorf("missing dismiss hint; got:\n%s", out)
	}
}

// TestRenderMissionModal_Rows asserts each child shows up as a row
// with its tier:role label and goal preview. The selected row gets
// the `▸ ` leader; other rows get `  `.
func TestRenderMissionModal_Rows(t *testing.T) {
	state := newMissionModalState(fakeLiveviewStatus(3))
	state.selected = 1
	out := renderMissionModal(state, 80)
	// All three rows present.
	for _, want := range []string{"mission:data-analyst", "mission:schema-explorer", "mission:report-builder"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing row %q in:\n%s", want, out)
		}
	}
	// Selected row carries the leader; row 0 stays unselected.
	// (snapshot order is sorted-by-id; childA → idx 0, childB → idx 1)
	if idx := strings.Index(out, "childB"); idx == -1 {
		t.Fatalf("childB row missing")
	}
	// Find row line for childB and confirm it starts with ▸ after any leading spaces.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "childB") && !strings.Contains(line, "▸") {
			t.Errorf("selected row missing ▸ leader: %q", line)
		}
	}
}

// TestMissionModal_Navigation — j/k/down/up move the selection
// within bounds; clamps at edges instead of wrapping.
func TestMissionModal_Navigation(t *testing.T) {
	cases := []struct {
		name  string
		start int
		key   string
		want  int
	}{
		{"j from 0", 0, "j", 1},
		{"down from 0", 0, "down", 1},
		{"j from last clamps", 2, "j", 2},
		{"k from 1", 1, "k", 0},
		{"up from 0 clamps", 0, "up", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := newMissionModalState(fakeLiveviewStatus(3))
			state.selected = tc.start
			tab := &tab{pendingMissionModal: state}
			tab.dispatchMissionModalKey(keyOf(tc.key))
			if state.selected != tc.want {
				t.Errorf("after %q: selected = %d; want %d", tc.key, state.selected, tc.want)
			}
		})
	}
}

// TestMissionModal_CancelSingle — `c` on the selected row submits
// `/cancel_subagent <id> /mission` via the tab's submit closure and
// flips the row's Cancelling flag for visual feedback.
func TestMissionModal_CancelSingle(t *testing.T) {
	m, submitted := newTestModel(t)
	cur := m.currentTab()
	cur.sidebarStatus = fakeLiveviewStatus(2)
	cur.pendingMissionModal = newMissionModalState(cur.sidebarStatus)
	cur.pendingMissionModal.selected = 1
	wantID := cur.pendingMissionModal.rows[1].SessionID

	cur.dispatchMissionModalKey(keyOf("c"))

	got := submitted.Load()
	if got == nil {
		t.Fatalf("nothing submitted")
	}
	sc, ok := (*got).(*protocol.SlashCommand)
	if !ok {
		t.Fatalf("submitted %T; want SlashCommand", *got)
	}
	if sc.Payload.Name != "cancel_subagent" {
		t.Errorf("slash name = %q; want cancel_subagent", sc.Payload.Name)
	}
	if len(sc.Payload.Args) < 1 || sc.Payload.Args[0] != wantID {
		t.Errorf("args = %v; want first arg = %q", sc.Payload.Args, wantID)
	}
	if !cur.pendingMissionModal.rows[1].Cancelling {
		t.Errorf("selected row's Cancelling flag not set")
	}
}

// TestMissionModal_CancelAll — Shift+C submits cancel_all_subagents
// and closes the modal so the operator sees the recentChildren trail
// take over.
func TestMissionModal_CancelAll(t *testing.T) {
	m, submitted := newTestModel(t)
	cur := m.currentTab()
	cur.sidebarStatus = fakeLiveviewStatus(3)
	cur.pendingMissionModal = newMissionModalState(cur.sidebarStatus)

	cur.dispatchMissionModalKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'C'}})

	got := submitted.Load()
	if got == nil {
		t.Fatalf("nothing submitted")
	}
	sc, ok := (*got).(*protocol.SlashCommand)
	if !ok {
		t.Fatalf("submitted %T; want SlashCommand", *got)
	}
	if sc.Payload.Name != "cancel_all_subagents" {
		t.Errorf("slash name = %q; want cancel_all_subagents", sc.Payload.Name)
	}
	if cur.pendingMissionModal != nil {
		t.Errorf("modal still open after cancel-all")
	}
}

// TestMissionModal_Dismiss — Esc clears the modal without submitting.
func TestMissionModal_Dismiss(t *testing.T) {
	m, submitted := newTestModel(t)
	cur := m.currentTab()
	cur.sidebarStatus = fakeLiveviewStatus(1)
	cur.pendingMissionModal = newMissionModalState(cur.sidebarStatus)

	cur.dispatchMissionModalKey(keyOf("esc"))

	if cur.pendingMissionModal != nil {
		t.Errorf("modal still open after esc")
	}
	if got := submitted.Load(); got != nil {
		t.Errorf("esc dismissed but submitted %T", *got)
	}
}

// TestSlashMission_OpensModal — typing `/mission` in the textarea
// and pressing Enter opens the modal instead of submitting a
// SlashCommand. The literal is captured client-side.
func TestSlashMission_OpensModal(t *testing.T) {
	m, submitted := newTestModel(t)
	m.currentTab().sidebarStatus = fakeLiveviewStatus(2)
	m.currentTab().textarea.SetValue("/mission")

	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = m2.(model)

	if m.currentTab().pendingMissionModal == nil {
		t.Fatalf("modal not opened")
	}
	if got := submitted.Load(); got != nil {
		t.Fatalf("/mission must NOT submit a frame; got %T", *got)
	}
}

// TestSlashMission_SuppressedDuringInquiry — opening `/mission`
// while a HITL inquiry is pending leaves the inquiry in place and
// flashes a status hint.
func TestSlashMission_SuppressedDuringInquiry(t *testing.T) {
	m, _ := newTestModel(t)
	cur := m.currentTab()
	cur.pendingInquiry = newInquiryState(&protocol.InquiryRequest{
		Payload: protocol.InquiryRequestPayload{
			Type:     protocol.InquiryTypeApproval,
			Question: "ok?",
		},
	})
	cur.openMissionModal()

	if cur.pendingMissionModal != nil {
		t.Errorf("mission modal opened despite pending inquiry")
	}
	if !strings.Contains(cur.statusLine, "inquiry pending") {
		t.Errorf("statusLine = %q; want inquiry-pending hint", cur.statusLine)
	}
}

// TestDoubleEsc_PanicCancel — two Esc presses within the window
// dispatch /cancel_all_subagents panic_cancel; outside the window
// nothing fires.
func TestDoubleEsc_PanicCancel(t *testing.T) {
	m, submitted := newTestModel(t)
	cur := m.currentTab()
	cur.sidebarStatus = fakeLiveviewStatus(2)

	// Controlled clock: first Esc at t=0, second at t=400ms (in window).
	clock := time.Now()
	m.nowFn = func() time.Time { return clock }

	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = m2.(model)
	if got := submitted.Load(); got != nil {
		t.Fatalf("single Esc submitted %T; want nothing", *got)
	}

	clock = clock.Add(400 * time.Millisecond)
	m.nowFn = func() time.Time { return clock }
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = m2.(model)

	got := submitted.Load()
	if got == nil {
		t.Fatalf("double-Esc within window did not fire panic-cancel")
	}
	sc, ok := (*got).(*protocol.SlashCommand)
	if !ok {
		t.Fatalf("submitted %T; want SlashCommand", *got)
	}
	if sc.Payload.Name != "cancel_all_subagents" {
		t.Errorf("slash = %q; want cancel_all_subagents", sc.Payload.Name)
	}
	if len(sc.Payload.Args) < 1 || sc.Payload.Args[0] != "panic_cancel" {
		t.Errorf("args = %v; want first arg panic_cancel", sc.Payload.Args)
	}
}

// TestDoubleEsc_OutsideWindow — second Esc more than escDoubleWindow
// after the first does NOT fire. Stamps a fresh tracker.
func TestDoubleEsc_OutsideWindow(t *testing.T) {
	m, submitted := newTestModel(t)
	cur := m.currentTab()
	cur.sidebarStatus = fakeLiveviewStatus(1)

	clock := time.Now()
	m.nowFn = func() time.Time { return clock }
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = m2.(model)

	clock = clock.Add(1500 * time.Millisecond) // > 800ms
	m.nowFn = func() time.Time { return clock }
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = m2.(model)

	if got := submitted.Load(); got != nil {
		t.Errorf("Esc-pause-Esc fired panic-cancel (submitted %T); should be silent", *got)
	}
}

// TestDoubleEsc_ResetsOnOtherKey — Esc, then any rune, then Esc
// does NOT trigger panic-cancel. Operator typing between dismissals
// is the canonical false-positive to avoid.
func TestDoubleEsc_ResetsOnOtherKey(t *testing.T) {
	m, submitted := newTestModel(t)
	cur := m.currentTab()
	cur.sidebarStatus = fakeLiveviewStatus(1)

	clock := time.Now()
	m.nowFn = func() time.Time { return clock }
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = m2.(model)
	// Any non-Esc key resets the tracker.
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m = m2.(model)
	clock = clock.Add(200 * time.Millisecond)
	m.nowFn = func() time.Time { return clock }
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = m2.(model)

	if got := submitted.Load(); got != nil {
		// The submitted frame might be the rune typed into the
		// textarea, which is fine — what we want to ensure is that
		// no SlashCommand cancel-all fired.
		if sc, ok := (*got).(*protocol.SlashCommand); ok && sc.Payload.Name == "cancel_all_subagents" {
			t.Errorf("Esc-key-Esc fired panic-cancel; should be reset by the intervening key")
		}
	}
}

// TestDoubleEsc_NoMissions — Esc-Esc when no missions are running
// flashes a status hint and does not submit anything.
func TestDoubleEsc_NoMissions(t *testing.T) {
	m, submitted := newTestModel(t)
	cur := m.currentTab()
	cur.sidebarStatus = &liveviewStatus{Depth: 0} // no children

	clock := time.Now()
	m.nowFn = func() time.Time { return clock }
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = m2.(model)
	clock = clock.Add(200 * time.Millisecond)
	m.nowFn = func() time.Time { return clock }
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = m2.(model)

	if got := submitted.Load(); got != nil {
		t.Errorf("Esc-Esc with no missions submitted %T; should be silent", *got)
	}
	if !strings.Contains(m.currentTab().statusLine, "no missions") {
		t.Errorf("statusLine = %q; want no-missions hint", m.currentTab().statusLine)
	}
}

// TestDoubleEsc_SuppressedWhenModal — Esc dismisses the mission
// modal; the second Esc does not arm because the first did not
// reach the tracker.
func TestDoubleEsc_SuppressedWhenModal(t *testing.T) {
	m, submitted := newTestModel(t)
	cur := m.currentTab()
	cur.sidebarStatus = fakeLiveviewStatus(2)
	cur.pendingMissionModal = newMissionModalState(cur.sidebarStatus)

	clock := time.Now()
	m.nowFn = func() time.Time { return clock }
	// First Esc — dismisses the modal via the tab dispatcher.
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = m2.(model)
	if m.currentTab().pendingMissionModal != nil {
		t.Fatalf("first Esc did not dismiss modal")
	}
	// Second Esc — within window — must NOT fire panic-cancel
	// because the first did not stamp the tracker.
	clock = clock.Add(200 * time.Millisecond)
	m.nowFn = func() time.Time { return clock }
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = m2.(model)
	if got := submitted.Load(); got != nil {
		t.Errorf("post-modal Esc-Esc fired %T; should be silent", *got)
	}
}

// TestDoubleEsc_StaleStampClearedByModalDismiss — operator presses
// Esc when no modal is up (stamps tracker), then a modal appears
// and gets dismissed with another Esc inside the window, then a
// third Esc still inside the original window arrives. Without the
// reset on modal-dismiss this third Esc would spuriously fire
// panic-cancel. Spec risk #2.
func TestDoubleEsc_StaleStampClearedByModalDismiss(t *testing.T) {
	m, submitted := newTestModel(t)
	cur := m.currentTab()
	cur.sidebarStatus = fakeLiveviewStatus(1)

	clock := time.Now()
	m.nowFn = func() time.Time { return clock }

	// 1. Esc with no modal → stamps lastEscAt.
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = m2.(model)

	// 2. Modal opens (simulate /mission), operator dismisses with Esc.
	clock = clock.Add(200 * time.Millisecond)
	m.nowFn = func() time.Time { return clock }
	m.currentTab().pendingMissionModal = newMissionModalState(cur.sidebarStatus)
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = m2.(model)

	// 3. Third Esc, still well within the original 800ms of step 1.
	clock = clock.Add(200 * time.Millisecond)
	m.nowFn = func() time.Time { return clock }
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = m2.(model)

	if got := submitted.Load(); got != nil {
		if sc, ok := (*got).(*protocol.SlashCommand); ok && sc.Payload.Name == "cancel_all_subagents" {
			t.Errorf("stale lastEscAt fired panic-cancel after modal dismiss")
		}
	}
}

// keyOf is a tiny helper for table tests that need a KeyMsg from a
// short string label. Handles letter runes ("j", "k", "c", "C") and
// named keys ("up", "down", "esc", "enter").
func keyOf(s string) tea.KeyMsg {
	switch s {
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// keep encoding/json referenced in case future tests need the
// payload introspection helpers.
var _ = json.Marshal
