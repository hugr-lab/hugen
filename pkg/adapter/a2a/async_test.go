package a2a

import (
	"context"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

func aref(id string) protocol.ActiveSubagentRef { return protocol.ActiveSubagentRef{SessionID: id} }

// finalFrame builds the turn-boundary consolidated AgentMessage (Final=true)
// carrying the A6 attribution fields. text is the consolidated body; the live
// chunk(s) the executor actually accumulates must precede it on the channel.
func finalFrame(root, text string, active, resultOf []protocol.ActiveSubagentRef) protocol.Frame {
	f := protocol.NewAgentMessageConsolidated(root, serviceParticipant(), text, 0, true, nil, "", "")
	f.Payload.ActiveAsync = active
	f.Payload.ResultOf = resultOf
	return f
}

// TestSessionExecutor_AsyncMission_HeldToCompleted drives the full A6 held path
// in one Execute: the ack turn's Final reports a newly-spawned async sub-agent
// (ActiveAsync) → the Task goes `working` and holds; the mission's auto-summary
// turn's Final carries ResultOf for it → the Task finishes `completed` with the
// summary.
func TestSessionExecutor_AsyncMission_HeldToCompleted(t *testing.T) {
	io := &fakeFrameIO{ch: make(chan protocol.Frame, 16)}
	reg := newContextRegistry(&fakeRootStore{}, quietLogger())
	e := newSessionExecutor(quietLogger(), reg, io, serviceParticipant(), nil)

	// Ack turn: a live chunk, then the Final reporting m1 newly async-spawned.
	io.ch <- idleFrame("root-1", "session_opened") // pre-turn, skipped
	io.ch <- agentFrame("root-1", "Started the analysis.", false, 0)
	io.ch <- finalFrame("root-1", "Started the analysis.", []protocol.ActiveSubagentRef{aref("m1")}, nil)
	// Later — the mission completes; the auto-summary turn's Final carries
	// ResultOf=[m1] and (m1 now gone) no live async.
	io.ch <- agentFrame("root-1", "Done: 42 rows.", false, 1)
	io.ch <- finalFrame("root-1", "Done: 42 rows.", nil, []protocol.ActiveSubagentRef{aref("m1")})

	execCtx := &a2asrv.ExecutorContext{
		Message:   a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("run an async analysis")),
		ContextID: "ctx-1",
		TaskID:    a2a.NewTaskID(),
	}
	events, err := collectErr(e.Execute(context.Background(), execCtx))
	if err != nil {
		t.Fatalf("async turn: %v", err)
	}
	// submitted task + working(ack) + completed(summary).
	if len(events) != 3 {
		t.Fatalf("yielded %d events, want 3 (submitted + working + completed)", len(events))
	}
	if _, ok := events[0].(*a2a.Task); !ok {
		t.Fatalf("event0 = %T, want *a2a.Task (materialise on going async)", events[0])
	}
	w, ok := events[1].(*a2a.TaskStatusUpdateEvent)
	if !ok || w.Status.State != a2a.TaskStateWorking {
		t.Fatalf("event1 = %T/%v, want working", events[1], w)
	}
	if got := messageText(w.Status.Message); !strings.Contains(got, "Started the analysis") {
		t.Errorf("working message = %q, want the ack text", got)
	}
	c, ok := events[2].(*a2a.TaskStatusUpdateEvent)
	if !ok || c.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("event2 = %T/%v, want completed", events[2], c)
	}
	got := messageText(c.Status.Message)
	if !strings.Contains(got, "42 rows") {
		t.Errorf("completed message = %q, want the summary", got)
	}
	if strings.Contains(got, "Started the analysis") {
		t.Errorf("completed message = %q, leaked the ack text into the summary", got)
	}
}

// TestSessionExecutor_SyncTurn_NotHeldAsAsync guards that a plain turn (Final
// with no ActiveAsync / ResultOf) finishes as a bare Message — A6 holding must
// not fire when nothing async was launched.
func TestSessionExecutor_SyncTurn_NotHeldAsAsync(t *testing.T) {
	io := &fakeFrameIO{ch: make(chan protocol.Frame, 8)}
	io.ch <- agentFrame("root-1", "hi", false, 0)
	io.ch <- finalFrame("root-1", "hi", nil, nil)

	reg := newContextRegistry(&fakeRootStore{}, quietLogger())
	e := newSessionExecutor(quietLogger(), reg, io, serviceParticipant(), nil)
	execCtx := &a2asrv.ExecutorContext{
		Message:   a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi")),
		ContextID: "ctx-1",
		TaskID:    a2a.NewTaskID(),
	}
	events, err := collectErr(e.Execute(context.Background(), execCtx))
	if err != nil {
		t.Fatalf("sync turn: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("yielded %d events, want 1 (bare message)", len(events))
	}
	if _, ok := events[0].(*a2a.Message); !ok {
		t.Errorf("event0 = %T, want *a2a.Message (no Task for a plain turn)", events[0])
	}
}

// TestSessionExecutor_ChatTurn_DuringRunningMission covers the shared-context
// race the contextSession.knownAsync diff solves: a second message arrives
// while an earlier Task holds m1. Its turn's Final still lists m1 as live
// (ActiveAsync), but m1 is already known — so this Task does NOT adopt it and
// finishes its own reply immediately.
func TestSessionExecutor_ChatTurn_DuringRunningMission(t *testing.T) {
	reg := newContextRegistry(&fakeRootStore{}, quietLogger())
	e := newSessionExecutor(quietLogger(), reg, &fakeFrameIO{}, serviceParticipant(), nil)
	// Simulate the earlier Task already owning m1.
	cs, _ := reg.resolve("ctx-1")
	cs.recordNewAsync([]protocol.ActiveSubagentRef{aref("m1")})

	io := &fakeFrameIO{ch: make(chan protocol.Frame, 8)}
	e.io = io
	io.ch <- agentFrame("root-1", "Here is the answer.", false, 0)
	io.ch <- finalFrame("root-1", "Here is the answer.", []protocol.ActiveSubagentRef{aref("m1")}, nil)

	execCtx := &a2asrv.ExecutorContext{
		Message:   a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("unrelated question")),
		ContextID: "ctx-1",
		TaskID:    a2a.NewTaskID(),
	}
	events, err := collectErr(e.Execute(context.Background(), execCtx))
	if err != nil {
		t.Fatalf("chat turn: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("yielded %d events, want 1 (bare message, not held for m1)", len(events))
	}
	if _, ok := events[0].(*a2a.Message); !ok {
		t.Errorf("event0 = %T, want *a2a.Message — must not hold for another Task's m1", events[0])
	}
}

// TestSessionExecutor_AsyncMission_InnerInquiryFlipsToInputRequired covers the
// A5×A6 interaction: an inquiry raised inside a running async mission flips the
// held `working` Task to `input-required` (one Task materialise), stashing the
// awaited set so the answering Execute can resume.
func TestSessionExecutor_AsyncMission_InnerInquiryFlipsToInputRequired(t *testing.T) {
	io := &fakeFrameIO{ch: make(chan protocol.Frame, 16)}
	reg := newContextRegistry(&fakeRootStore{}, quietLogger())
	e := newSessionExecutor(quietLogger(), reg, io, serviceParticipant(), nil)

	io.ch <- agentFrame("root-1", "Started.", false, 0)
	io.ch <- finalFrame("root-1", "Started.", []protocol.ActiveSubagentRef{aref("m1")}, nil) // → working
	io.ch <- inquiryFrame("root-1", "req-9", "ses-child", protocol.InquiryTypeClarification, "Which dataset?")

	execCtx := &a2asrv.ExecutorContext{
		Message:   a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("run async")),
		ContextID: "ctx-1",
		TaskID:    a2a.NewTaskID(),
	}
	events, err := collectErr(e.Execute(context.Background(), execCtx))
	if err != nil {
		t.Fatalf("async+inquiry turn: %v", err)
	}
	// submitted + working + input-required — exactly ONE Task materialise.
	if len(events) != 3 {
		t.Fatalf("yielded %d events, want 3 (submitted + working + input-required)", len(events))
	}
	tasks := 0
	for _, ev := range events {
		if _, ok := ev.(*a2a.Task); ok {
			tasks++
		}
	}
	if tasks != 1 {
		t.Errorf("materialised %d Tasks, want exactly 1 (taskBorn respected on the flip)", tasks)
	}
	last, ok := events[2].(*a2a.TaskStatusUpdateEvent)
	if !ok || last.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("event2 = %T/%v, want input-required", events[2], last)
	}
	cs, _ := reg.resolve("ctx-1")
	pend := cs.peekPending()
	if pend == nil {
		t.Fatal("the inner inquiry did not park the context")
	}
	// The awaited async set is stashed so the answering Execute resumes the hold.
	if len(pend.AsyncAwaited) != 1 || pend.AsyncAwaited[0] != "m1" {
		t.Errorf("parked AsyncAwaited = %v, want [m1]", pend.AsyncAwaited)
	}
}

// TestSessionExecutor_AnswerResumesAsyncHold covers the resume leg: the Execute
// that answers an inner inquiry restores the stashed awaited set, reports
// `working`, and holds until the mission's ResultOf lands → completed.
func TestSessionExecutor_AnswerResumesAsyncHold(t *testing.T) {
	io := &fakeFrameIO{ch: make(chan protocol.Frame, 16)}
	reg := newContextRegistry(&fakeRootStore{}, quietLogger())
	e := newSessionExecutor(quietLogger(), reg, io, serviceParticipant(), nil)
	cs, _ := reg.resolve("ctx-1")
	cs.recordNewAsync([]protocol.ActiveSubagentRef{aref("m1")}) // m1 known (the held Task owns it)
	cs.park(&parkedInquiry{
		RequestID: "req-9", CallerSessionID: "ses-child",
		Kind: protocol.InquiryTypeClarification, Question: "Which dataset?",
		AsyncAwaited: []string{"m1"},
	})

	// After the answer the mission completes → auto-summary Final with ResultOf=[m1].
	io.ch <- agentFrame("root-1", "Done: EMEA.", false, 0)
	io.ch <- finalFrame("root-1", "Done: EMEA.", nil, []protocol.ActiveSubagentRef{aref("m1")})

	execCtx := &a2asrv.ExecutorContext{
		Message:   a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("the sales dataset")),
		ContextID: "ctx-1",
		TaskID:    a2a.NewTaskID(),
	}
	events, err := collectErr(e.Execute(context.Background(), execCtx))
	if err != nil {
		t.Fatalf("answer-resume turn: %v", err)
	}
	// submitted(on resume working) + working(answer accepted) + completed(summary).
	if len(events) != 3 {
		t.Fatalf("yielded %d events, want 3 (submitted + working + completed); got %d", len(events), len(events))
	}
	if _, ok := events[0].(*a2a.Task); !ok {
		t.Fatalf("event0 = %T, want *a2a.Task", events[0])
	}
	if w, ok := events[1].(*a2a.TaskStatusUpdateEvent); !ok || w.Status.State != a2a.TaskStateWorking {
		t.Fatalf("event1 = %T/%v, want working (answer accepted, still holding)", events[1], events[1])
	}
	c, ok := events[2].(*a2a.TaskStatusUpdateEvent)
	if !ok || c.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("event2 = %T/%v, want completed", events[2], events[2])
	}
	if got := messageText(c.Status.Message); !strings.Contains(got, "EMEA") {
		t.Errorf("completed message = %q, want the summary after resume", got)
	}
	// The answer went down as an InquiryResponse and the pending was cleared.
	if cs.peekPending() != nil {
		t.Error("pending not cleared after answering")
	}
	resp, ok := io.submitted[0].(*protocol.InquiryResponse)
	if !ok {
		t.Fatalf("submitted[0] = %T, want *protocol.InquiryResponse", io.submitted[0])
	}
	if resp.Payload.Response != "the sales dataset" {
		t.Errorf("inquiry response = %q, want the answer text", resp.Payload.Response)
	}
}
