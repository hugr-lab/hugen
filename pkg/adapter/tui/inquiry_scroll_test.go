package tui

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// bigApprovalInquiry returns an approval request whose Question is
// tall enough (many lines) to overrun any sane modal height budget,
// standing in for a large mission plan + AC diff.
func bigApprovalInquiry() *protocol.InquiryRequest {
	// `<<N>>` markers are substring-collision-free (<<1>> is not a
	// substring of <<10>>) so assertions can pin an exact line.
	var q strings.Builder
	q.WriteString("Approve this plan:\n")
	for i := 1; i <= 60; i++ {
		fmt.Fprintf(&q, "ac-%d acceptance criterion marker <<%d>>\n", i, i)
	}
	return &protocol.InquiryRequest{
		BaseFrame: protocol.BaseFrame{Session: "sess-big"},
		Payload: protocol.InquiryRequestPayload{
			RequestID: "req-big",
			Type:      protocol.InquiryTypeApproval,
			Question:  q.String(),
			Context:   "A long plan that does not fit the terminal.",
		},
	}
}

// TestRenderInquiryModal_ClampsToBudgetAndPinsFooter verifies that a
// tall approval modal is bounded to maxHeight, surfaces a scroll
// indicator, and STILL shows the action footer (the four choices +
// hint) — the keys must never scroll off. Phase 6.x review follow-up.
func TestRenderInquiryModal_ClampsToBudgetAndPinsFooter(t *testing.T) {
	state := newInquiryState(bigApprovalInquiry())
	const maxHeight = 20

	out := renderInquiryModal(state, 80, maxHeight)

	if h := lipgloss.Height(out); h > maxHeight {
		t.Errorf("modal height %d exceeds budget %d:\n%s", h, maxHeight, out)
	}
	for _, want := range []string{
		"approve + auto-tools", // footer choice — pinned
		"[a]", "[n]", "[r]", // footer keys — pinned
		"PgUp/PgDn to scroll", // scroll indicator present
	} {
		if !strings.Contains(out, want) {
			t.Errorf("clamped modal missing pinned/indicator %q in:\n%s", want, out)
		}
	}
	// The far-tail criterion can't be visible at scroll 0.
	if strings.Contains(out, "<<60>>") {
		t.Errorf("tail line should be below the fold at scroll=0:\n%s", out)
	}
}

// TestRenderInquiryModal_ScrollMovesWindow drives bodyScroll and
// asserts the visible window shifts: a line near the top leaves view
// and a later line enters. Over-scrolling clamps instead of blanking.
func TestRenderInquiryModal_ScrollMovesWindow(t *testing.T) {
	state := newInquiryState(bigApprovalInquiry())
	const maxHeight = 20

	top := renderInquiryModal(state, 80, maxHeight)
	if !strings.Contains(top, "<<1>>") {
		t.Fatalf("expected first criterion visible at scroll=0:\n%s", top)
	}

	// bodyScroll=25 puts body line 25 (= ac-23, since the title +
	// blank + "Approve this plan:" occupy lines 0-2) at the window
	// top — so <<23>> is the first visible criterion and <<1>> is gone.
	state.bodyScroll = 25
	mid := renderInquiryModal(state, 80, maxHeight)
	if strings.Contains(mid, "<<1>>") {
		t.Errorf("first criterion should have scrolled out of view:\n%s", mid)
	}
	if !strings.Contains(mid, "<<23>>") {
		t.Errorf("expected the criterion at the scroll offset in view:\n%s", mid)
	}

	// Over-scroll: the renderer clamps bodyScroll to the last window
	// (tail visible, height still bounded).
	state.bodyScroll = 9999
	bot := renderInquiryModal(state, 80, maxHeight)
	if h := lipgloss.Height(bot); h > maxHeight {
		t.Errorf("over-scrolled modal height %d exceeds budget %d", h, maxHeight)
	}
	if !strings.Contains(bot, "<<60>>") {
		t.Errorf("tail criterion should be visible after max scroll:\n%s", bot)
	}
	if state.bodyScroll > 60 {
		t.Errorf("bodyScroll not clamped at render: %d", state.bodyScroll)
	}
}

// TestComposeModalInner_UnboundedRendersFull confirms maxHeight<=0
// disables clamping (full body + footer, no indicator) and resets a
// stale scroll offset — the path unit tests and pre-size callers hit.
func TestComposeModalInner_UnboundedRendersFull(t *testing.T) {
	state := &inquiryState{bodyScroll: 7}
	body := "line-a\nline-b\nline-c"
	footer := "the-hint"

	out := composeModalInner(state, body, footer, 0, 40)

	for _, want := range []string{"line-a", "line-b", "line-c", "the-hint"} {
		if !strings.Contains(out, want) {
			t.Errorf("unbounded inner missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "PgUp/PgDn") {
		t.Errorf("unbounded inner should carry no scroll indicator:\n%s", out)
	}
	if state.bodyScroll != 0 {
		t.Errorf("bodyScroll = %d, want reset to 0 when content fits", state.bodyScroll)
	}
}

// TestRenderBody_ModalNeverOverflows is the end-to-end guard: a tall
// modal stacked on a full chat viewport must keep the whole body
// (chat + modal + footer) within the height budget. Pre-fix this
// overflowed the terminal by the modal's excess rows.
func TestRenderBody_ModalNeverOverflows(t *testing.T) {
	tb := newTab("ses-overflow", protocol.ParticipantInfo{ID: "u"},
		func(protocol.Frame) error { return nil }, slog.Default())
	tb.viewport.Width = 80
	tb.viewport.Height = 30
	// Fill the chat with content so the viewport renders full-height.
	tb.viewport.SetContent(strings.Repeat("chat scrollback line\n", 60))
	tb.pendingInquiry = newInquiryState(bigApprovalInquiry())

	const availHeight = 36 // terminal height minus the tab bar
	body := tb.renderBody(80, 80, false, availHeight)

	if h := lipgloss.Height(body); h > availHeight {
		t.Errorf("rendered body height %d exceeds availHeight %d (modal overflow)", h, availHeight)
	}
	// Footer choices still present — the modal didn't get clipped to
	// nothing.
	if !strings.Contains(body, "approve + auto-tools") {
		t.Errorf("modal action footer missing from bounded body:\n%s", body)
	}
}

// TestApprovalKeys_PgDownPgUpScroll verifies PgDn/PgUp move the
// modal's bodyScroll (handled, not falling through to the textarea)
// and that PgUp floors at 0. The render-time clamp owns the upper
// bound, so the handler only needs the step + floor.
func TestApprovalKeys_PgDownPgUpScroll(t *testing.T) {
	tb := newTab("ses-keys", protocol.ParticipantInfo{ID: "u"},
		func(protocol.Frame) error { return nil }, slog.Default())
	pend := newInquiryState(bigApprovalInquiry())
	tb.pendingInquiry = pend

	if handled, _ := tb.dispatchApprovalChoiceKey(pend, tea.KeyMsg{Type: tea.KeyPgDown}); !handled {
		t.Fatal("PgDown not handled by the approval modal")
	}
	if pend.bodyScroll != modalScrollStep {
		t.Errorf("bodyScroll = %d after PgDown, want %d", pend.bodyScroll, modalScrollStep)
	}

	tb.dispatchApprovalChoiceKey(pend, tea.KeyMsg{Type: tea.KeyPgDown})
	if pend.bodyScroll != 2*modalScrollStep {
		t.Errorf("bodyScroll = %d after second PgDown, want %d", pend.bodyScroll, 2*modalScrollStep)
	}

	// Three PgUps from 2 steps must floor at 0, not go negative.
	for i := 0; i < 3; i++ {
		tb.dispatchApprovalChoiceKey(pend, tea.KeyMsg{Type: tea.KeyPgUp})
	}
	if pend.bodyScroll != 0 {
		t.Errorf("bodyScroll = %d after flooring PgUps, want 0", pend.bodyScroll)
	}
}
