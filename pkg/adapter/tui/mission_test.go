package tui

import (
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

// TestMissionModal_Rebuild_DropsCompletedRows — when a liveview
// status frame arrives mid-modal-open, rebuild() refreshes the row
// list from the fresh projection. Rows whose children finished
// (and thus disappeared from liveview.Children) drop out;
// Cancelling flags for still-present rows persist so the operator
// keeps seeing "⊘ cancelling…" feedback. Phase 5.x.skill-polish-1
// R1/R2 fix.
func TestMissionModal_Rebuild_DropsCompletedRows(t *testing.T) {
	state := newMissionModalState(fakeLiveviewStatus(3))
	state.selected = 1
	state.markCancelling(0)
	state.markCancelling(2)

	// Live projection now shows only one of the original three
	// children (the others terminated).
	survivors := &liveviewStatus{
		SessionID: "root-aaaaaa01",
		Depth:     0,
		Children:  map[string]*liveviewStatus{},
		ChildMeta: map[string]childMetaEntry{},
	}
	keepID := state.rows[1].SessionID // childB...
	survivors.Children[keepID] = &liveviewStatus{SessionID: keepID, Depth: 1}
	survivors.ChildMeta[keepID] = childMetaEntry{Role: "data-analyst", Task: "goal B"}

	state.rebuild(survivors)

	if len(state.rows) != 1 {
		t.Fatalf("rebuilt rows = %d; want 1", len(state.rows))
	}
	if state.rows[0].SessionID != keepID {
		t.Errorf("kept row id = %q; want %q", state.rows[0].SessionID, keepID)
	}
	// childB wasn't in the cancelling map, so flag should be clean.
	if state.rows[0].Cancelling {
		t.Errorf("Cancelling flag leaked onto preserved row")
	}
	if state.selected != 0 {
		t.Errorf("selected = %d; want 0 (clamped after rebuild)", state.selected)
	}
}

// TestMissionModal_Rebuild_PreservesCancellingFlag — when a row
// survives a rebuild and was marked Cancelling, the flag persists
// so the "⊘ cancelling…" UI stays stable while the cancel
// SessionClose round-trip completes.
func TestMissionModal_Rebuild_PreservesCancellingFlag(t *testing.T) {
	state := newMissionModalState(fakeLiveviewStatus(2))
	state.markCancelling(0)
	keepID := state.rows[0].SessionID

	// Same children, fresh projection (mimics a liveview status
	// frame arriving while cancel is in flight).
	state.rebuild(fakeLiveviewStatus(2))

	var found bool
	for _, r := range state.rows {
		if r.SessionID == keepID {
			found = true
			if !r.Cancelling {
				t.Errorf("Cancelling flag lost on rebuild")
			}
		}
	}
	if !found {
		t.Errorf("kept id missing after rebuild")
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
// named keys ("up", "down", "esc", "enter", "backspace").
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
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// Phase 5.2 ζ — /mission modal parked-row actions.

// fakeLiveviewWithParked builds a liveview projection with two
// children: index 0 is live (Active), index 1 is parked
// (awaiting_dismissal) with the given parkedAt timestamp.
func fakeLiveviewWithParked(parkedAt time.Time) *liveviewStatus {
	s := &liveviewStatus{
		SessionID: "root-aaaaaa01",
		Depth:     0,
		Children:  map[string]*liveviewStatus{},
		ChildMeta: map[string]childMetaEntry{},
	}
	live := "childA00000001"
	parked := "childB00000001"
	s.Children[live] = &liveviewStatus{
		SessionID:      live,
		Depth:          1,
		LifecycleState: protocol.SessionStatusActive,
	}
	s.Children[parked] = &liveviewStatus{
		SessionID:      parked,
		Depth:          1,
		LifecycleState: protocol.SessionStatusAwaitingDismissal,
		ParkedAt:       parkedAt,
	}
	s.ChildMeta[live] = childMetaEntry{Role: "data-analyst", Skill: "analyst", Task: "goal A",
		StartedAt: time.Now().Add(-2 * time.Minute)}
	s.ChildMeta[parked] = childMetaEntry{Role: "data-chatter", Skill: "data-chat", Task: "сколько платежей",
		StartedAt: time.Now().Add(-3 * time.Minute)}
	return s
}

// TestSnapshotMissions_ParkedBadgePropagates verifies the snapshot
// flips Parked + ParkedAt for awaiting_dismissal children and
// leaves live children untouched.
func TestSnapshotMissions_ParkedBadgePropagates(t *testing.T) {
	parkedAt := time.Now().Add(-30 * time.Second)
	state := newMissionModalState(fakeLiveviewWithParked(parkedAt))
	if len(state.rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(state.rows))
	}
	// snapshotMissions sorts by id: childA → 0 (live), childB → 1 (parked).
	if state.rows[0].Parked {
		t.Errorf("row 0 marked Parked; want false (live child)")
	}
	if !state.rows[1].Parked {
		t.Errorf("row 1 not marked Parked; want true")
	}
	if !state.rows[1].ParkedAt.Equal(parkedAt) {
		t.Errorf("row 1 ParkedAt = %v; want %v", state.rows[1].ParkedAt, parkedAt)
	}
}

// TestRenderMissionModal_ParkedBadge verifies the rendered modal
// surfaces the "⏸ parked …" badge for parked rows and the new key
// hints land in the footer.
func TestRenderMissionModal_ParkedBadge(t *testing.T) {
	state := newMissionModalState(fakeLiveviewWithParked(time.Now().Add(-5 * time.Second)))
	out := renderMissionModal(state, 140)
	if !strings.Contains(out, "⏸") {
		t.Errorf("missing parked badge glyph; got:\n%s", out)
	}
	if !strings.Contains(out, "parked") {
		t.Errorf("missing 'parked' label; got:\n%s", out)
	}
	if !strings.Contains(out, "[d] dismiss parked") {
		t.Errorf("missing d-key hint; got:\n%s", out)
	}
	if !strings.Contains(out, "[f] follow up") {
		t.Errorf("missing f-key hint; got:\n%s", out)
	}
}

// TestMissionModal_DismissParkedRow — `d` on a parked row submits
// /dismiss_subagent and flips Dismissing for visual feedback.
func TestMissionModal_DismissParkedRow(t *testing.T) {
	m, submitted := newTestModel(t)
	cur := m.currentTab()
	cur.sidebarStatus = fakeLiveviewWithParked(time.Now().Add(-10 * time.Second))
	cur.pendingMissionModal = newMissionModalState(cur.sidebarStatus)
	cur.pendingMissionModal.selected = 1 // parked row
	wantID := cur.pendingMissionModal.rows[1].SessionID

	cur.dispatchMissionModalKey(keyOf("d"))

	got := submitted.Load()
	if got == nil {
		t.Fatalf("nothing submitted")
	}
	sc, ok := (*got).(*protocol.SlashCommand)
	if !ok {
		t.Fatalf("submitted %T; want SlashCommand", *got)
	}
	if sc.Payload.Name != "dismiss_subagent" {
		t.Errorf("slash name = %q; want dismiss_subagent", sc.Payload.Name)
	}
	if len(sc.Payload.Args) < 1 || sc.Payload.Args[0] != wantID {
		t.Errorf("args = %v; want first arg = %q", sc.Payload.Args, wantID)
	}
	if !cur.pendingMissionModal.rows[1].Dismissing {
		t.Errorf("parked row Dismissing flag not set after `d`")
	}
}

// TestMissionModal_DismissLiveRowRejected — `d` on a non-parked row
// must not dispatch and must surface a hint instead.
func TestMissionModal_DismissLiveRowRejected(t *testing.T) {
	m, submitted := newTestModel(t)
	cur := m.currentTab()
	cur.sidebarStatus = fakeLiveviewWithParked(time.Now())
	cur.pendingMissionModal = newMissionModalState(cur.sidebarStatus)
	cur.pendingMissionModal.selected = 0 // live row

	cur.dispatchMissionModalKey(keyOf("d"))

	if got := submitted.Load(); got != nil {
		t.Errorf("submit fired on non-parked row; got %T", *got)
	}
	if cur.pendingMissionModal.transientHint == "" {
		t.Errorf("transientHint not set on live-row reject")
	}
	if cur.pendingMissionModal.rows[0].Dismissing {
		t.Errorf("Dismissing flag set on live row")
	}
}

// TestMissionModal_FollowupOpensSubstate — `f` on a parked row
// switches the modal to follow-up mode with the target captured.
func TestMissionModal_FollowupOpensSubstate(t *testing.T) {
	state := newMissionModalState(fakeLiveviewWithParked(time.Now()))
	state.selected = 1
	tab := &tab{pendingMissionModal: state}
	tab.dispatchMissionModalKey(keyOf("f"))
	if state.mode != missionModeFollowup {
		t.Errorf("mode = %v; want missionModeFollowup", state.mode)
	}
	if state.followupTarget != state.rows[1].SessionID {
		t.Errorf("followupTarget = %q; want %q", state.followupTarget, state.rows[1].SessionID)
	}
}

// TestMissionModal_FollowupRejectsLiveRow — `f` on a non-parked row
// surfaces the same hint as `d` and does not switch mode.
func TestMissionModal_FollowupRejectsLiveRow(t *testing.T) {
	state := newMissionModalState(fakeLiveviewWithParked(time.Now()))
	state.selected = 0
	tab := &tab{pendingMissionModal: state}
	tab.dispatchMissionModalKey(keyOf("f"))
	if state.mode == missionModeFollowup {
		t.Errorf("mode flipped to follow-up on live row")
	}
	if state.transientHint == "" {
		t.Errorf("hint not surfaced on live-row reject")
	}
}

// TestMissionModal_FollowupEnterDispatches — typing chars + Enter
// submits /notify_subagent with the captured target and the buffer.
func TestMissionModal_FollowupEnterDispatches(t *testing.T) {
	m, submitted := newTestModel(t)
	cur := m.currentTab()
	cur.sidebarStatus = fakeLiveviewWithParked(time.Now())
	cur.pendingMissionModal = newMissionModalState(cur.sidebarStatus)
	cur.pendingMissionModal.selected = 1
	wantID := cur.pendingMissionModal.rows[1].SessionID

	cur.dispatchMissionModalKey(keyOf("f"))
	for _, r := range "hello" {
		cur.dispatchMissionModalKey(keyOf(string(r)))
	}
	cur.dispatchMissionModalKey(keyOf("enter"))

	got := submitted.Load()
	if got == nil {
		t.Fatalf("nothing submitted")
	}
	sc, ok := (*got).(*protocol.SlashCommand)
	if !ok {
		t.Fatalf("submitted %T; want SlashCommand", *got)
	}
	if sc.Payload.Name != "notify_subagent" {
		t.Errorf("slash name = %q; want notify_subagent", sc.Payload.Name)
	}
	if len(sc.Payload.Args) < 2 || sc.Payload.Args[0] != wantID || sc.Payload.Args[1] != "hello" {
		t.Errorf("args = %v; want [%q, \"hello\"]", sc.Payload.Args, wantID)
	}
	if cur.pendingMissionModal.mode != missionModeList {
		t.Errorf("mode = %v; want list (exitFollowup after dispatch)", cur.pendingMissionModal.mode)
	}
}

// TestMissionModal_FollowupEscReturns — esc in the follow-up
// substate returns to the list without submitting.
func TestMissionModal_FollowupEscReturns(t *testing.T) {
	m, submitted := newTestModel(t)
	cur := m.currentTab()
	cur.sidebarStatus = fakeLiveviewWithParked(time.Now())
	cur.pendingMissionModal = newMissionModalState(cur.sidebarStatus)
	cur.pendingMissionModal.selected = 1

	cur.dispatchMissionModalKey(keyOf("f"))
	cur.dispatchMissionModalKey(keyOf("x"))
	cur.dispatchMissionModalKey(keyOf("esc"))

	if got := submitted.Load(); got != nil {
		t.Errorf("submit fired on esc; got %T", *got)
	}
	if cur.pendingMissionModal == nil {
		t.Fatalf("modal closed; want still open")
	}
	if cur.pendingMissionModal.mode != missionModeList {
		t.Errorf("mode = %v; want list", cur.pendingMissionModal.mode)
	}
}

// TestMissionModal_FollowupBackspaceTrims — backspace deletes one
// rune from the buffer; ctrl+u clears it entirely.
func TestMissionModal_FollowupBufferEdit(t *testing.T) {
	state := newMissionModalState(fakeLiveviewWithParked(time.Now()))
	state.selected = 1
	tab := &tab{pendingMissionModal: state}
	tab.dispatchMissionModalKey(keyOf("f"))
	for _, r := range "abcde" {
		tab.dispatchMissionModalKey(keyOf(string(r)))
	}
	if state.followupBuf != "abcde" {
		t.Fatalf("buf = %q; want abcde", state.followupBuf)
	}
	tab.dispatchMissionModalKey(keyOf("backspace"))
	if state.followupBuf != "abcd" {
		t.Errorf("backspace: buf = %q; want abcd", state.followupBuf)
	}
	tab.dispatchMissionModalKey(tea.KeyMsg{Type: tea.KeyCtrlU})
	if state.followupBuf != "" {
		t.Errorf("ctrl+u: buf = %q; want empty", state.followupBuf)
	}
}

// TestMissionModal_FollowupTargetVanishedExits — when the parked
// child terminates while the operator is mid-typing, the rebuild
// path drops the substate back to list with a hint.
func TestMissionModal_FollowupTargetVanishedExits(t *testing.T) {
	state := newMissionModalState(fakeLiveviewWithParked(time.Now()))
	state.selected = 1
	tab := &tab{pendingMissionModal: state}
	tab.dispatchMissionModalKey(keyOf("f"))
	tab.dispatchMissionModalKey(keyOf("a"))
	if state.mode != missionModeFollowup {
		t.Fatalf("follow-up not entered")
	}
	// Rebuild from a projection that drops the parked child.
	survivors := &liveviewStatus{
		SessionID: "root-aaaaaa01",
		Depth:     0,
		Children:  map[string]*liveviewStatus{},
		ChildMeta: map[string]childMetaEntry{},
	}
	survivors.Children[state.rows[0].SessionID] = &liveviewStatus{
		SessionID: state.rows[0].SessionID, Depth: 1, LifecycleState: protocol.SessionStatusActive,
	}
	survivors.ChildMeta[state.rows[0].SessionID] = childMetaEntry{Role: "data-analyst", Task: "goal A"}
	state.rebuild(survivors)
	if state.mode != missionModeList {
		t.Errorf("mode = %v; want list after target vanished", state.mode)
	}
	if state.transientHint == "" {
		t.Errorf("hint not surfaced after follow-up target vanished")
	}
}

