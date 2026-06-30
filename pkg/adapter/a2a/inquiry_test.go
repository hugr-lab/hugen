package a2a

import (
	"context"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// inquiryFrame builds an InquiryRequest the way it lands on root's outbox.
func inquiryFrame(root, reqID, caller, typ, question string) protocol.Frame {
	return protocol.NewInquiryRequest(root, serviceParticipant(), protocol.InquiryRequestPayload{
		RequestID:       reqID,
		CallerSessionID: caller,
		Type:            typ,
		Question:        question,
	})
}

func TestParseApprovalAnswer(t *testing.T) {
	cases := []struct {
		line          string
		wantApproved  bool
		wantWithTools bool
		wantReason    string
		wantOK        bool
	}{
		{"approve", true, false, "", true},
		{"/approve looks good", true, false, "looks good", true},
		{"yes", true, false, "", true},
		{"approve with tools", true, true, "", true},
		{"approve all", true, true, "", true},
		{"deny not safe", false, false, "not safe", true},
		{"no", false, false, "", true},
		{"reject", false, false, "", true},
		{"actually can you also add X", false, false, "", false}, // free-form → refine
		{"", false, false, "", false},
	}
	for _, c := range cases {
		approved, withTools, reason, ok := parseApprovalAnswer(c.line)
		if ok != c.wantOK || approved != c.wantApproved || withTools != c.wantWithTools || reason != c.wantReason {
			t.Errorf("parseApprovalAnswer(%q) = (%v,%v,%q,%v), want (%v,%v,%q,%v)",
				c.line, approved, withTools, reason, ok, c.wantApproved, c.wantWithTools, c.wantReason, c.wantOK)
		}
	}
}

func TestBuildInquiryResponse_Approval(t *testing.T) {
	pend := &parkedInquiry{RequestID: "req-1", CallerSessionID: "caller-1", Kind: protocol.InquiryTypeApproval}

	// Clear approve.
	resp, err := buildInquiryResponse(serviceParticipant(), "root-1", pend, "approve")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if resp.SessionID() != "root-1" {
		t.Errorf("addressed to %q, want root-1", resp.SessionID())
	}
	if resp.Payload.RequestID != "req-1" || resp.Payload.CallerSessionID != "caller-1" {
		t.Errorf("routing fields not preserved: %+v", resp.Payload)
	}
	if resp.Payload.Approved == nil || !*resp.Payload.Approved {
		t.Errorf("Approved = %v, want true", resp.Payload.Approved)
	}
	if resp.Payload.AutoApproveTools {
		t.Error("plain approve must not auto-approve tools")
	}

	// Approve with tools.
	resp, _ = buildInquiryResponse(serviceParticipant(), "root-1", pend, "approve with tools")
	if resp.Payload.Approved == nil || !*resp.Payload.Approved || !resp.Payload.AutoApproveTools {
		t.Errorf("approve-with-tools: Approved=%v AutoApproveTools=%v, want true/true",
			resp.Payload.Approved, resp.Payload.AutoApproveTools)
	}

	// Deny with reason.
	resp, _ = buildInquiryResponse(serviceParticipant(), "root-1", pend, "deny too risky")
	if resp.Payload.Approved == nil || *resp.Payload.Approved {
		t.Errorf("deny: Approved = %v, want false", resp.Payload.Approved)
	}
	if resp.Payload.Reason != "too risky" {
		t.Errorf("deny reason = %q, want %q", resp.Payload.Reason, "too risky")
	}

	// Free-form text on an approval → refine (Approved=nil + Response).
	resp, _ = buildInquiryResponse(serviceParticipant(), "root-1", pend, "also include the EU region")
	if resp.Payload.Approved != nil {
		t.Errorf("refine: Approved = %v, want nil", resp.Payload.Approved)
	}
	if resp.Payload.Response != "also include the EU region" {
		t.Errorf("refine Response = %q, want the free-form text", resp.Payload.Response)
	}

	// Empty approval answer is an error (re-ask).
	if _, err := buildInquiryResponse(serviceParticipant(), "root-1", pend, "   "); err == nil {
		t.Error("empty approval answer did not error")
	}
}

func TestBuildInquiryResponse_Clarification(t *testing.T) {
	pend := &parkedInquiry{RequestID: "req-2", CallerSessionID: "caller-2", Kind: protocol.InquiryTypeClarification}

	resp, err := buildInquiryResponse(serviceParticipant(), "root-1", pend, "  the EMEA region  ")
	if err != nil {
		t.Fatalf("clarification: %v", err)
	}
	if resp.Payload.Response != "the EMEA region" {
		t.Errorf("Response = %q, want trimmed text", resp.Payload.Response)
	}

	if _, err := buildInquiryResponse(serviceParticipant(), "root-1", pend, ""); err == nil {
		t.Error("empty clarification answer did not error")
	}
}

func TestBuildInquiryResponse_ResearchBatchCollapsesToResponse(t *testing.T) {
	pend := &parkedInquiry{RequestID: "req-3", CallerSessionID: "caller-3", Kind: protocol.InquiryTypeResearchBatch}
	resp, err := buildInquiryResponse(serviceParticipant(), "root-1", pend, "use last quarter, group by region")
	if err != nil {
		t.Fatalf("research_batch: %v", err)
	}
	if resp.Payload.Response != "use last quarter, group by region" {
		t.Errorf("Response = %q, want the free-form line", resp.Payload.Response)
	}
}

func TestBuildInquiryResponse_UnknownKind(t *testing.T) {
	pend := &parkedInquiry{RequestID: "req-4", Kind: "weird"}
	if _, err := buildInquiryResponse(serviceParticipant(), "root-1", pend, "x"); err == nil {
		t.Error("unknown inquiry kind did not error")
	}
}

func TestInquiryPrompt(t *testing.T) {
	// Approval with preamble + context + the reply grammar.
	p := &protocol.InquiryRequestPayload{
		Type:     protocol.InquiryTypeApproval,
		Question: "Approve this plan?",
		Context:  "Plan: query sales, render report.",
	}
	got := inquiryPrompt(p, "Here is what I'll do.")
	for _, want := range []string{"Here is what I'll do.", "Approve this plan?", "Plan: query sales", "approve with tools"} {
		if !strings.Contains(got, want) {
			t.Errorf("approval prompt missing %q:\n%s", want, got)
		}
	}

	// Clarification with options listed.
	p = &protocol.InquiryRequestPayload{
		Type:     protocol.InquiryTypeClarification,
		Question: "Which region?",
		Options:  []string{"EMEA", "APAC"},
	}
	got = inquiryPrompt(p, "")
	for _, want := range []string{"Which region?", "EMEA", "APAC"} {
		if !strings.Contains(got, want) {
			t.Errorf("clarification prompt missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "approve with tools") {
		t.Error("clarification prompt should not carry the approval grammar")
	}

	// Empty question falls back to a default.
	p = &protocol.InquiryRequestPayload{Type: protocol.InquiryTypeClarification}
	if got := inquiryPrompt(p, ""); got != "Input required." {
		t.Errorf("empty clarification prompt = %q, want fallback", got)
	}
}

// TestSessionExecutor_InquiryRoundTrip drives the full A5 round-trip across two
// Execute calls sharing one durable context: turn 1 surfaces an approval as an
// input-required task and parks; turn 2's inbound "approve" is routed down as an
// InquiryResponse (not a new user turn) and the turn completes.
func TestSessionExecutor_InquiryRoundTrip(t *testing.T) {
	io := &fakeFrameIO{ch: make(chan protocol.Frame, 8)}
	reg := newContextRegistry(&fakeRootStore{}, quietLogger())
	e := newSessionExecutor(quietLogger(), reg, io, serviceParticipant(), nil)

	// --- Turn 1: the session asks for approval ---
	io.ch <- idleFrame("root-1", "session_opened") // pre-turn, must be skipped
	io.ch <- inquiryFrame("root-1", "req-1", "root-1", protocol.InquiryTypeApproval, "Approve the plan?")
	execCtx1 := &a2asrv.ExecutorContext{
		Message:   a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("do the thing")),
		ContextID: "ctx-1",
		TaskID:    a2a.NewTaskID(),
	}
	events1, err := collectErr(e.Execute(context.Background(), execCtx1))
	if err != nil {
		t.Fatalf("turn1: %v", err)
	}
	// New task must be materialised (submitted) before the input-required status.
	if len(events1) != 2 {
		t.Fatalf("turn1 yielded %d events, want 2 (submitted task + input-required)", len(events1))
	}
	if _, ok := events1[0].(*a2a.Task); !ok {
		t.Fatalf("turn1 event0 = %T, want *a2a.Task (materialise)", events1[0])
	}
	upd, ok := events1[1].(*a2a.TaskStatusUpdateEvent)
	if !ok {
		t.Fatalf("turn1 event1 = %T, want *a2a.TaskStatusUpdateEvent", events1[1])
	}
	if upd.Status.State != a2a.TaskStateInputRequired {
		t.Errorf("turn1 state = %q, want input-required", upd.Status.State)
	}
	if got := messageText(upd.Status.Message); !strings.Contains(got, "Approve the plan?") {
		t.Errorf("input-required prompt = %q, want to contain the question", got)
	}
	// Turn 1 submitted only the user_message.
	if len(io.submitted) != 1 {
		t.Fatalf("turn1 submitted %d frames, want 1 (user_message)", len(io.submitted))
	}
	if _, ok := io.submitted[0].(*protocol.UserMessage); !ok {
		t.Fatalf("turn1 submitted[0] = %T, want *protocol.UserMessage", io.submitted[0])
	}
	cs, _ := reg.resolve("ctx-1")
	if cs.peekPending() == nil {
		t.Fatal("context not parked after the inquiry frame")
	}

	// --- Turn 2: the user answers "approve" ---
	io.ch <- agentFrame("root-1", "Done.", true, 0)
	io.ch <- idleFrame("root-1", "turn_complete")
	execCtx2 := &a2asrv.ExecutorContext{
		Message:   a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("approve")),
		ContextID: "ctx-1",
		TaskID:    a2a.NewTaskID(),
	}
	events2, err := collectErr(e.Execute(context.Background(), execCtx2))
	if err != nil {
		t.Fatalf("turn2: %v", err)
	}
	// Copilot-style continuation (no StoredTask) finishes with a bare Message.
	if len(events2) != 1 {
		t.Fatalf("turn2 yielded %d events, want 1 (final message)", len(events2))
	}
	if _, ok := events2[0].(*a2a.Message); !ok {
		t.Fatalf("turn2 event0 = %T, want *a2a.Message", events2[0])
	}
	// Turn 2 submitted an InquiryResponse, NOT a second user_message.
	if len(io.submitted) != 2 {
		t.Fatalf("submitted total = %d, want 2 (user_message + inquiry_response)", len(io.submitted))
	}
	resp, ok := io.submitted[1].(*protocol.InquiryResponse)
	if !ok {
		t.Fatalf("turn2 submitted[1] = %T, want *protocol.InquiryResponse", io.submitted[1])
	}
	if resp.SessionID() != "root-1" {
		t.Errorf("response addressed to %q, want root-1 (root cascades down)", resp.SessionID())
	}
	if resp.Payload.RequestID != "req-1" || resp.Payload.CallerSessionID != "root-1" {
		t.Errorf("response routing = %+v, want req-1 / root-1", resp.Payload)
	}
	if resp.Payload.Approved == nil || !*resp.Payload.Approved {
		t.Errorf("response Approved = %v, want true", resp.Payload.Approved)
	}
	if cs.peekPending() != nil {
		t.Error("pending not cleared after a valid answer")
	}
}

// TestSessionExecutor_Answer_CompletesStoredTask covers the spec-compliant
// client that carries the taskId on the continuation: once a task exists the SDK
// rejects a bare Message, so the answer turn must finish via a completed status.
func TestSessionExecutor_Answer_CompletesStoredTask(t *testing.T) {
	io := &fakeFrameIO{ch: make(chan protocol.Frame, 4)}
	reg := newContextRegistry(&fakeRootStore{}, quietLogger())
	e := newSessionExecutor(quietLogger(), reg, io, serviceParticipant(), nil)
	cs, _ := reg.resolve("ctx-1")
	cs.park(&parkedInquiry{RequestID: "req-1", CallerSessionID: "root-1", Kind: protocol.InquiryTypeClarification, Question: "Which region?"})

	io.ch <- agentFrame("root-1", "Thanks.", true, 0)
	io.ch <- idleFrame("root-1", "turn_complete")
	execCtx := &a2asrv.ExecutorContext{
		Message:    a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("EMEA")),
		ContextID:  "ctx-1",
		TaskID:     a2a.NewTaskID(),
		StoredTask: &a2a.Task{ID: "task-1", ContextID: "ctx-1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}},
	}
	events, err := collectErr(e.Execute(context.Background(), execCtx))
	if err != nil {
		t.Fatalf("answer turn: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("yielded %d events, want 1 (completed status)", len(events))
	}
	upd, ok := events[0].(*a2a.TaskStatusUpdateEvent)
	if !ok {
		t.Fatalf("event0 = %T, want *a2a.TaskStatusUpdateEvent (bare Message rejected after task stored)", events[0])
	}
	if upd.Status.State != a2a.TaskStateCompleted {
		t.Errorf("state = %q, want completed", upd.Status.State)
	}
	resp := io.submitted[0].(*protocol.InquiryResponse)
	if resp.Payload.Response != "EMEA" {
		t.Errorf("clarification Response = %q, want EMEA", resp.Payload.Response)
	}
}

// TestSessionExecutor_ReAsksOnEmptyAnswer verifies an unparseable answer keeps
// the inquiry parked and re-emits input-required (the session is still blocked
// in session:inquire — submitting a user turn would be wrong).
func TestSessionExecutor_ReAsksOnEmptyAnswer(t *testing.T) {
	io := &fakeFrameIO{ch: make(chan protocol.Frame, 4)}
	reg := newContextRegistry(&fakeRootStore{}, quietLogger())
	e := newSessionExecutor(quietLogger(), reg, io, serviceParticipant(), nil)
	cs, _ := reg.resolve("ctx-1")
	cs.park(&parkedInquiry{RequestID: "req-1", CallerSessionID: "root-1", Kind: protocol.InquiryTypeApproval, Question: "Approve?"})

	execCtx := &a2asrv.ExecutorContext{
		Message:   a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("   ")),
		ContextID: "ctx-1",
		TaskID:    a2a.NewTaskID(),
	}
	events, err := collectErr(e.Execute(context.Background(), execCtx))
	if err != nil {
		t.Fatalf("re-ask turn: %v", err)
	}
	// Re-ask materialises a task + re-emits input-required.
	if len(events) != 2 {
		t.Fatalf("yielded %d events, want 2 (submitted + re-asked input-required)", len(events))
	}
	upd, ok := events[1].(*a2a.TaskStatusUpdateEvent)
	if !ok || upd.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("re-ask event = %T/%v, want input-required status", events[1], upd)
	}
	// No frame submitted to the session (still blocked in session:inquire).
	if len(io.submitted) != 0 {
		t.Errorf("submitted %d frames on re-ask, want 0", len(io.submitted))
	}
	if cs.peekPending() == nil {
		t.Error("pending cleared on an unparseable answer; should stay parked")
	}
}
