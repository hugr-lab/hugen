package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

func approvalInquiry() *protocol.InquiryRequest {
	return &protocol.InquiryRequest{
		BaseFrame: protocol.BaseFrame{Session: "sess-root"},
		Payload: protocol.InquiryRequestPayload{
			RequestID:       "req-1",
			CallerSessionID: "worker-71",
			Type:            protocol.InquiryTypeApproval,
			Question:        "Run bash.shell: rm -rf /tmp/cache",
			Context:         "Cleaning up between iterations of the schema-discovery pass.",
		},
	}
}

func clarificationInquiry() *protocol.InquiryRequest {
	return &protocol.InquiryRequest{
		BaseFrame: protocol.BaseFrame{Session: "sess-root"},
		Payload: protocol.InquiryRequestPayload{
			RequestID:       "req-2",
			CallerSessionID: "mission-3",
			Type:            protocol.InquiryTypeClarification,
			Question:        "Which dataset should I prioritise?",
			Options:         []string{"northwind", "chinook"},
		},
	}
}

func TestRenderInquiryModal_ApprovalContainsHintAndQuestion(t *testing.T) {
	state := newInquiryState(approvalInquiry())
	out := renderInquiryModal(state, 60)
	for _, want := range []string{
		"Approval required",
		"worker-7", // session id truncated by shortID() — 8 chars
		"Run bash.shell",
		"approve",
		"deny",
		"reply with reason",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("modal missing %q in:\n%s", want, out)
		}
	}
}

func TestRenderInquiryModal_ClarificationStartsInReplyMode(t *testing.T) {
	state := newInquiryState(clarificationInquiry())
	if !state.replyMode {
		t.Fatal("clarification should auto-enter reply mode (textarea is the only input path)")
	}
	out := renderInquiryModal(state, 60)
	if !strings.Contains(out, "Clarification needed") {
		t.Errorf("missing title in clarification modal: %s", out)
	}
	if !strings.Contains(out, "northwind") || !strings.Contains(out, "chinook") {
		t.Errorf("missing options in clarification modal: %s", out)
	}
	if !strings.Contains(out, "type answer") {
		t.Errorf("missing reply hint: %s", out)
	}
}

func TestModel_InquiryRequest_PopulatesPendingInquiry(t *testing.T) {
	m, _ := newTestModel(t)
	m2, _ := m.Update(frameMsg{frame: approvalInquiry()})
	m = m2.(model)
	if m.pendingInquiry == nil {
		t.Fatal("pendingInquiry should be set after InquiryRequest")
	}
	if m.pendingInquiry.req.RequestID != "req-1" {
		t.Errorf("RequestID = %q", m.pendingInquiry.req.RequestID)
	}
	view := m.View()
	if !strings.Contains(view, "Approval required") {
		t.Errorf("View missing inquiry modal:\n%s", view)
	}
}

func TestModel_InquiryApprove_YKeySubmitsApproved(t *testing.T) {
	m, submitted := newTestModel(t)
	m2, _ := m.Update(frameMsg{frame: approvalInquiry()})
	m = m2.(model)

	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	m = m2.(model)

	if m.pendingInquiry != nil {
		t.Errorf("pendingInquiry should be cleared after y submit; got %+v", m.pendingInquiry)
	}
	f := submitted.Load()
	if f == nil {
		t.Fatal("nothing submitted")
	}
	resp, ok := (*f).(*protocol.InquiryResponse)
	if !ok {
		t.Fatalf("submitted frame is %T, want *InquiryResponse", *f)
	}
	if resp.Payload.Approved == nil || !*resp.Payload.Approved {
		t.Errorf("Approved = %v; want true", resp.Payload.Approved)
	}
	if resp.Payload.CallerSessionID != "worker-71" {
		t.Errorf("CallerSessionID = %q; want worker-71", resp.Payload.CallerSessionID)
	}
}

func TestModel_InquiryDeny_NKeySubmitsDenied(t *testing.T) {
	m, submitted := newTestModel(t)
	m2, _ := m.Update(frameMsg{frame: approvalInquiry()})
	m = m2.(model)
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	m = m2.(model)
	if m.pendingInquiry != nil {
		t.Errorf("modal should be cleared after n submit")
	}
	f := submitted.Load()
	resp := (*f).(*protocol.InquiryResponse)
	if resp.Payload.Approved == nil || *resp.Payload.Approved {
		t.Errorf("Approved = %v; want false", resp.Payload.Approved)
	}
}

func TestModel_InquiryReplyMode_RKeyEntersReplyMode(t *testing.T) {
	m, _ := newTestModel(t)
	m2, _ := m.Update(frameMsg{frame: approvalInquiry()})
	m = m2.(model)
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	m = m2.(model)
	if m.pendingInquiry == nil || !m.pendingInquiry.replyMode {
		t.Fatalf("r should enter replyMode; got %+v", m.pendingInquiry)
	}
	if m.pendingInquiry.replyVerb != "approve" {
		t.Errorf("replyVerb default = %q; want approve", m.pendingInquiry.replyVerb)
	}
	view := m.View()
	if !strings.Contains(view, "type reason") {
		t.Errorf("reply-mode hint missing in:\n%s", view)
	}
}

func TestModel_InquiryClarification_EnterSubmitsResponse(t *testing.T) {
	m, submitted := newTestModel(t)
	m2, _ := m.Update(frameMsg{frame: clarificationInquiry()})
	m = m2.(model)
	// Type "northwind" into the textarea.
	for _, r := range "northwind" {
		m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = m2.(model)
	}
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = m2.(model)
	if m.pendingInquiry != nil {
		t.Errorf("modal should clear after enter; got %+v", m.pendingInquiry)
	}
	f := submitted.Load()
	if f == nil {
		t.Fatal("no frame submitted")
	}
	resp, ok := (*f).(*protocol.InquiryResponse)
	if !ok {
		t.Fatalf("got %T, want *InquiryResponse", *f)
	}
	if resp.Payload.Response != "northwind" {
		t.Errorf("Response = %q; want northwind", resp.Payload.Response)
	}
}

func TestModel_InquiryResponseEcho_ClearsPendingInquiry(t *testing.T) {
	m, _ := newTestModel(t)
	m2, _ := m.Update(frameMsg{frame: approvalInquiry()})
	m = m2.(model)
	approved := true
	echo := &protocol.InquiryResponse{
		Payload: protocol.InquiryResponsePayload{
			RequestID: "req-1",
			Approved:  &approved,
		},
	}
	m2, _ = m.Update(frameMsg{frame: echo})
	m = m2.(model)
	if m.pendingInquiry != nil {
		t.Errorf("echo InquiryResponse should clear modal; got %+v", m.pendingInquiry)
	}
}

func TestModel_InquiryEsc_DismissesAndSurfacesBanner(t *testing.T) {
	m, _ := newTestModel(t)
	m2, _ := m.Update(frameMsg{frame: approvalInquiry()})
	m = m2.(model)
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = m2.(model)
	if m.pendingInquiry != nil {
		t.Errorf("esc should dismiss modal; got %+v", m.pendingInquiry)
	}
	if !strings.Contains(m.bannerError, "still pending") {
		t.Errorf("expected dismiss banner; got %q", m.bannerError)
	}
}

func TestWrap_PacksWordsByWidth(t *testing.T) {
	in := "The quick brown fox jumps over the lazy dog"
	got := wrap(in, 12)
	for _, line := range strings.Split(got, "\n") {
		if len(line) > 12 {
			t.Errorf("line longer than 12: %q (len=%d)", line, len(line))
		}
	}
	// Sanity: must still contain the whole content.
	if !strings.Contains(strings.ReplaceAll(got, "\n", " "), "lazy dog") {
		t.Errorf("wrap dropped content: %q", got)
	}
}
