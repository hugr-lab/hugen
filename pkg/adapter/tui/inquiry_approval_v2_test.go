package tui

import (
	"strings"
	"sync/atomic"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// TestApprovalModalV2_AKeySubmitsApproveWithTools covers the new
// §4.6 "approve with tools" path: lowercase `a` submits a payload
// with Approved=*true AND AutoApproveTools=true so the mission ext
// will stamp the per-mission policy hook flag. Without this on-wire
// bit the runtime can't distinguish "approve" from "approve with
// tools" and degrades silently to plain approve.
func TestApprovalModalV2_AKeySubmitsApproveWithTools(t *testing.T) {
	m, submitted := newTestModel(t)
	m2, _ := m.Update(frameMsg{frame: approvalInquiry()})
	m = m2.(model)
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m = m2.(model)
	if m.currentTab().pendingInquiry != nil {
		t.Fatalf("modal should clear after `a` submit; got %+v", m.currentTab().pendingInquiry)
	}
	f := submitted.Load()
	if f == nil {
		t.Fatal("no frame submitted")
	}
	resp, ok := (*f).(*protocol.InquiryResponse)
	if !ok {
		t.Fatalf("submitted = %T, want *InquiryResponse", *f)
	}
	if resp.Payload.Approved == nil || !*resp.Payload.Approved {
		t.Errorf("Approved = %v; want *true", resp.Payload.Approved)
	}
	if !resp.Payload.AutoApproveTools {
		t.Errorf("AutoApproveTools = false; want true (this is the whole point of `a`)")
	}
}

// TestApprovalModalV2_ShiftAKeySubmitsApprove covers the plain-
// approve path. The on-wire shape must carry AutoApproveTools=false
// (omitempty drops the key entirely) so the runtime preserves
// per-tool modal behaviour.
func TestApprovalModalV2_ShiftAKeySubmitsApprove(t *testing.T) {
	m, submitted := newTestModel(t)
	m2, _ := m.Update(frameMsg{frame: approvalInquiry()})
	m = m2.(model)
	// Bubble Tea reports Shift+a as the rune 'A'.
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'A'}})
	m = m2.(model)
	resp := lastSubmittedInquiryResponse(t, submitted)
	if resp.Payload.Approved == nil || !*resp.Payload.Approved {
		t.Errorf("Approved = %v; want *true", resp.Payload.Approved)
	}
	if resp.Payload.AutoApproveTools {
		t.Errorf("AutoApproveTools = true on plain-approve; want false")
	}
}

// TestApprovalModalV2_YKeyLegacyAliasApprove preserves backwards
// compatibility per §4.6.9: `y` remains a valid keystroke and maps
// to plain approve (not approve-with-tools — operators upgrading
// from the prior single-keystroke modal shouldn't accidentally
// grant blanket tool approval).
func TestApprovalModalV2_YKeyLegacyAliasApprove(t *testing.T) {
	m, submitted := newTestModel(t)
	m2, _ := m.Update(frameMsg{frame: approvalInquiry()})
	m = m2.(model)
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	m = m2.(model)
	resp := lastSubmittedInquiryResponse(t, submitted)
	if resp.Payload.Approved == nil || !*resp.Payload.Approved {
		t.Errorf("Approved = %v; want *true (`y` aliases plain approve)", resp.Payload.Approved)
	}
	if resp.Payload.AutoApproveTools {
		t.Errorf("AutoApproveTools = true on `y` legacy alias; want false")
	}
}

// TestApprovalModalV2_NKeySubmitsReject covers the reject path:
// Approved=*false and the runtime treats this as mission abort.
// Reason stays empty for the immediate-keystroke flow; operators
// who want to attach a reason can use refine.
func TestApprovalModalV2_NKeySubmitsReject(t *testing.T) {
	m, submitted := newTestModel(t)
	m2, _ := m.Update(frameMsg{frame: approvalInquiry()})
	m = m2.(model)
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	m = m2.(model)
	resp := lastSubmittedInquiryResponse(t, submitted)
	if resp.Payload.Approved == nil || *resp.Payload.Approved {
		t.Errorf("Approved = %v; want *false", resp.Payload.Approved)
	}
	if resp.Payload.AutoApproveTools {
		t.Errorf("AutoApproveTools must NOT leak on reject; got true")
	}
}

// TestApprovalModalV2_RKeyOpensRefineTextareaThenSubmits covers
// the refine path: `r` opens the textarea (no immediate submit),
// the user types feedback, Enter submits with Approved=nil +
// Response=feedback (the wire shape mission's
// interpretValidateApprovalResponse reads as planner refinement).
func TestApprovalModalV2_RKeyOpensRefineTextareaThenSubmits(t *testing.T) {
	m, submitted := newTestModel(t)
	m2, _ := m.Update(frameMsg{frame: approvalInquiry()})
	m = m2.(model)
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	m = m2.(model)
	if m.currentTab().pendingInquiry == nil || !m.currentTab().pendingInquiry.replyMode {
		t.Fatal("`r` should drop into reply mode (textarea opens)")
	}
	if m.currentTab().pendingInquiry.replyVerb != "refine" {
		t.Fatalf("replyVerb = %q, want refine", m.currentTab().pendingInquiry.replyVerb)
	}
	if submitted.Load() != nil {
		t.Fatal("`r` must NOT submit immediately; submit waits for Enter on textarea")
	}
	// Type the feedback.
	for _, r := range "narrow scope to schema X" {
		m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = m2.(model)
	}
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = m2.(model)
	resp := lastSubmittedInquiryResponse(t, submitted)
	if resp.Payload.Approved != nil {
		t.Errorf("Approved = %v; want nil (refine wire shape)", resp.Payload.Approved)
	}
	if resp.Payload.Response != "narrow scope to schema X" {
		t.Errorf("Response = %q; want feedback text", resp.Payload.Response)
	}
	if resp.Payload.AutoApproveTools {
		t.Errorf("AutoApproveTools must NOT leak on refine; got true")
	}
}

// TestApprovalModalV2_RefineRejectsEmptyFeedback prevents an empty
// Enter from accidentally submitting an empty Response — the
// planner can't make a useful next iteration from no signal.
func TestApprovalModalV2_RefineRejectsEmptyFeedback(t *testing.T) {
	m, submitted := newTestModel(t)
	m2, _ := m.Update(frameMsg{frame: approvalInquiry()})
	m = m2.(model)
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	m = m2.(model)
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // no text typed
	m = m2.(model)
	if submitted.Load() != nil {
		t.Fatal("empty-feedback Enter should not submit")
	}
	if m.currentTab().pendingInquiry == nil || !m.currentTab().pendingInquiry.replyMode {
		t.Fatal("modal should still be in reply mode after empty-Enter reject")
	}
	if m.currentTab().bannerError == "" {
		t.Errorf("expected a banner error explaining the rejection")
	}
}

// TestApprovalModalV2_DigitShortcutsCommit verifies the digit
// path: pressing `1` through `4` jumps to the matching row and
// commits in one keystroke. `1` is approve-with-tools (first row).
func TestApprovalModalV2_DigitShortcutsCommit(t *testing.T) {
	m, submitted := newTestModel(t)
	m2, _ := m.Update(frameMsg{frame: approvalInquiry()})
	m = m2.(model)
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'1'}})
	m = m2.(model)
	resp := lastSubmittedInquiryResponse(t, submitted)
	if resp.Payload.Approved == nil || !*resp.Payload.Approved {
		t.Errorf("Approved = %v; want *true on `1` (approve-with-tools)", resp.Payload.Approved)
	}
	if !resp.Payload.AutoApproveTools {
		t.Errorf("AutoApproveTools = false on `1`; want true (first row maps to approve-with-tools)")
	}
}

// TestApprovalModalV2_NavThenEnterCommitsHighlight covers the
// j/k + Enter path: operators who prefer list-style nav over
// direct shortcuts can move the highlight then commit. Confirms
// the highlight + Enter path works and respects the row mapping.
func TestApprovalModalV2_NavThenEnterCommitsHighlight(t *testing.T) {
	m, submitted := newTestModel(t)
	m2, _ := m.Update(frameMsg{frame: approvalInquiry()})
	m = m2.(model)
	// Move j twice → highlight on row 2 (index 2, "reject").
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m = m2.(model)
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m = m2.(model)
	if got := m.currentTab().pendingInquiry.approvalHighlight; got != 2 {
		t.Fatalf("approvalHighlight = %d after j/j; want 2", got)
	}
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = m2.(model)
	resp := lastSubmittedInquiryResponse(t, submitted)
	if resp.Payload.Approved == nil || *resp.Payload.Approved {
		t.Errorf("Approved = %v; want *false (reject after nav to row 2)", resp.Payload.Approved)
	}
}

// TestApprovalModalV2_KKeyWrapsBackward verifies the wrap-around:
// pressing `k` from the default (row 0) lands on the last row
// rather than going negative. Matches research_batch widget
// conventions.
func TestApprovalModalV2_KKeyWrapsBackward(t *testing.T) {
	m, _ := newTestModel(t)
	m2, _ := m.Update(frameMsg{frame: approvalInquiry()})
	m = m2.(model)
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	m = m2.(model)
	if got := m.currentTab().pendingInquiry.approvalHighlight; got != 3 {
		t.Errorf("approvalHighlight = %d after k from row 0; want 3 (last row)", got)
	}
}

// TestApprovalModalV2_RendererMarksHighlightedRow verifies the
// renderer emits the active marker (`▸ `) only on the highlighted
// row. Without this the operator can't tell which row Enter would
// commit.
func TestApprovalModalV2_RendererMarksHighlightedRow(t *testing.T) {
	state := newInquiryState(approvalInquiry())
	// Default highlight is row 0 (approve with tools). Move to
	// row 2 (reject) to make the test deterministic across any
	// future default-highlight tweaks.
	state.approvalHighlight = 2
	out := renderInquiryModal(state, 80, 0)
	if !strings.Contains(out, "▸ [n]") {
		t.Errorf("expected highlight marker `▸ [n]` on reject row; got:\n%s", out)
	}
	if strings.Contains(out, "▸ [a]") || strings.Contains(out, "▸ [A]") || strings.Contains(out, "▸ [r]") {
		t.Errorf("highlight marker leaked onto idle rows in:\n%s", out)
	}
}

// lastSubmittedInquiryResponse pulls the last submitted frame and
// asserts it's an InquiryResponse — the harness's helpers don't
// offer a built-in waiter, so tests reach in directly.
func lastSubmittedInquiryResponse(t *testing.T, submitted *atomic.Pointer[protocol.Frame]) *protocol.InquiryResponse {
	t.Helper()
	f := submitted.Load()
	if f == nil {
		t.Fatal("no frame submitted")
	}
	resp, ok := (*f).(*protocol.InquiryResponse)
	if !ok {
		t.Fatalf("submitted frame = %T, want *InquiryResponse", *f)
	}
	return resp
}
