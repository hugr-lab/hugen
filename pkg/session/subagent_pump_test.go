package session

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

// newChildStub builds a minimal *Session usable as the "child" in
// pump tests: only the fields the pump touches (id, out) are filled.
// The session has no Run loop, no deps, no inbox — tests push frames
// directly into the outbox channel and close it to terminate the pump.
func newChildStub(id string, buf int) *Session {
	if buf <= 0 {
		buf = 8
	}
	return &Session{
		id:  id,
		out: make(chan protocol.Frame, buf),
	}
}

// agentParticipant is a small constructor for the author stamped on
// frames the test pushes into the child's outbox. Real children use
// their session.agent.Participant(); for the pump tests author is
// load-bearing only as part of the BaseFrame envelope.
func agentParticipant(id string) protocol.ParticipantInfo {
	return protocol.ParticipantInfo{ID: id, Kind: protocol.ParticipantAgent}
}

// resultCapture holds projected SubagentResults observed via a tool
// feed registered on the parent. The feed callback runs on the
// parent's Run loop goroutine; tests read the slice from the test
// goroutine — the mutex guards that handoff.
type resultCapture struct {
	mu  sync.Mutex
	all []*protocol.SubagentResult
}

func (rc *resultCapture) add(sr *protocol.SubagentResult) {
	rc.mu.Lock()
	rc.all = append(rc.all, sr)
	rc.mu.Unlock()
}

func (rc *resultCapture) snapshot() []*protocol.SubagentResult {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	out := make([]*protocol.SubagentResult, len(rc.all))
	copy(out, rc.all)
	return out
}

func (rc *resultCapture) len() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return len(rc.all)
}

// captureSubagentResults installs a feed on parent that records every
// projected SubagentResult into the returned struct. Caller defers
// the release closure. Must be called BEFORE starting the pump; the
// feed reads activeToolFeed via atomic load on every routed frame.
func captureSubagentResults(parent *Session) (*resultCapture, func()) {
	rc := &resultCapture{}
	feed := &ToolFeed{
		Consumes: func(k protocol.Kind) bool {
			return k == protocol.KindSubagentResult
		},
		Feed: func(f protocol.Frame) {
			if sr, ok := f.(*protocol.SubagentResult); ok {
				rc.add(sr)
			}
		},
		BlockingState:  protocol.SessionStatusWaitSubagents,
		BlockingReason: "test=pump_capture",
	}
	release := parent.registerToolFeed(context.Background(), feed)
	return rc, release
}

// waitFor polls fn at 5ms ticks until it returns true or the deadline
// fires. Unit tests use it to wait on the pump goroutine's effect to
// land in shared state without the brittleness of fixed-duration sleeps.
func waitFor(t *testing.T, fn func() bool, deadline time.Duration) {
	t.Helper()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	timeout := time.After(deadline)
	for {
		if fn() {
			return
		}
		select {
		case <-tick.C:
		case <-timeout:
			t.Fatalf("waitFor: condition not met within %v", deadline)
		}
	}
}

// TestPump_ProjectFinalConsolidatedAgentMessage verifies the canonical
// path: child emits one terminal AgentMessage{Final:true,Consolidated:true},
// pump translates it into a parent-side SubagentResult that lands on
// parent's tool feed. Reason is "completed", Result mirrors the model's
// final text, TurnsUsed reflects the consolidated count seen.
func TestPump_ProjectFinalConsolidatedAgentMessage(t *testing.T) {
	parent, cleanup := newTestParent(t, withTestRunLoop())
	defer cleanup()

	captured, release := captureSubagentResults(parent)
	defer release()

	child := newChildStub("child-1", 8)
	go parent.consumeChildOutbox(child)

	// One non-final consolidated row first (a tool-iteration retire),
	// then the turn-final consolidated row. consolidatedSeen=2.
	child.out <- protocol.NewAgentMessageConsolidated(child.id, agentParticipant("a1"),
		"using a tool", 0, false, nil, "", "")
	child.out <- protocol.NewAgentMessageConsolidated(child.id, agentParticipant("a1"),
		"the answer is 42", 1, true, nil, "", "")
	close(child.out)

	waitFor(t, func() bool { return captured.len() >= 1 }, 2*time.Second)

	if captured.len() != 1 {
		t.Fatalf("captured = %d, want 1", captured.len())
	}
	sr := captured.snapshot()[0]
	if sr.Payload.Reason != protocol.TerminationCompleted {
		t.Errorf("reason = %q, want %q", sr.Payload.Reason, protocol.TerminationCompleted)
	}
	if sr.Payload.Result != "the answer is 42" {
		t.Errorf("result = %q, want %q", sr.Payload.Result, "the answer is 42")
	}
	if sr.Payload.TurnsUsed != 2 {
		t.Errorf("turns = %d, want 2", sr.Payload.TurnsUsed)
	}
	if sr.SessionID() != parent.id {
		t.Errorf("sr.SessionID = %q, want parent.id %q", sr.SessionID(), parent.id)
	}
	if sr.FromSessionID() != child.id {
		t.Errorf("sr.FromSession = %q, want child.id %q", sr.FromSessionID(), child.id)
	}
}

// TestPump_ProjectTerminalError verifies the error path: child emits
// Error{Recoverable:false}, pump translates into SubagentResult with
// reason="error: <code>" and result=<message>.
func TestPump_ProjectTerminalError(t *testing.T) {
	parent, cleanup := newTestParent(t, withTestRunLoop())
	defer cleanup()

	captured, release := captureSubagentResults(parent)
	defer release()

	child := newChildStub("child-err", 4)
	go parent.consumeChildOutbox(child)

	child.out <- protocol.NewError(child.id, agentParticipant("a1"),
		"stream_error", "connection reset", false)
	close(child.out)

	waitFor(t, func() bool { return captured.len() >= 1 }, 2*time.Second)

	sr := captured.snapshot()[0]
	if sr.Payload.Reason != "error: stream_error" {
		t.Errorf("reason = %q, want %q", sr.Payload.Reason, "error: stream_error")
	}
	if sr.Payload.Result != "connection reset" {
		t.Errorf("result = %q, want %q", sr.Payload.Result, "connection reset")
	}
}

// TestPump_RecoverableErrorAlsoProjects asserts that even Recoverable
// errors (stream_error / 429 / transient model failures that the
// session-side path leaves as Recoverable=true) project as terminal
// from a subagent's outbox. A subagent has no human user to retry
// against — leaving the child idle hangs the parent's wait_subagents
// forever. Retries belong in the model layer; once Error reaches
// session.emit the model has already given up. Parent's LLM decides
// the next step from the projected SubagentResult.
func TestPump_RecoverableErrorAlsoProjects(t *testing.T) {
	parent, cleanup := newTestParent(t, withTestRunLoop())
	defer cleanup()

	captured, release := captureSubagentResults(parent)
	defer release()

	child := newChildStub("child-rec", 4)
	go parent.consumeChildOutbox(child)

	child.out <- protocol.NewError(child.id, agentParticipant("a1"),
		"stream_error", "Anthropic API error (status 429): rate_limit_error", true)
	close(child.out)

	waitFor(t, func() bool { return captured.len() >= 1 }, 2*time.Second)

	got := captured.snapshot()
	if len(got) != 1 {
		t.Fatalf("captured = %+v, want exactly one projection", projectionReasons(got))
	}
	sr := got[0]
	if sr.Payload.Reason != "error: stream_error" {
		t.Errorf("reason = %q, want %q", sr.Payload.Reason, "error: stream_error")
	}
	if sr.Payload.Result == "" {
		t.Errorf("result is empty, want the underlying error message")
	}
}

// TestPump_StreamingChunksAreDrained verifies the high-volume drain
// path that fixes the original phase-4.1b hang: streaming chunks
// (Consolidated=false) and reasoning frames flow through without
// stalling and without surfacing as projections.
func TestPump_StreamingChunksAreDrained(t *testing.T) {
	parent, cleanup := newTestParent(t, withTestRunLoop())
	defer cleanup()

	captured, release := captureSubagentResults(parent)
	defer release()

	child := newChildStub("child-stream", 4) // small buffer — pump must drain
	go parent.consumeChildOutbox(child)

	for i := 0; i < 50; i++ {
		child.out <- protocol.NewAgentMessage(child.id, agentParticipant("a1"),
			"chunk", i, false)
	}
	for i := 0; i < 10; i++ {
		child.out <- protocol.NewReasoning(child.id, agentParticipant("a1"),
			"thinking", i, false)
	}
	// Close without any consolidated/final frame.
	close(child.out)

	// Post-loop finalizer fires abnormal_close.
	waitFor(t, func() bool { return captured.len() >= 1 }, 2*time.Second)
	if captured.snapshot()[0].Payload.Reason != "abnormal_close" {
		t.Errorf("expected abnormal_close, got %q", captured.snapshot()[0].Payload.Reason)
	}
}

// TestPump_SessionTerminatedFallback covers the path where child's
// outbox carries a terminal SessionTerminated (best-effort emitClose
// in handleExit). Without a prior Final-projection, pump uses the
// terminated row's reason/result.
func TestPump_SessionTerminatedFallback(t *testing.T) {
	parent, cleanup := newTestParent(t, withTestRunLoop())
	defer cleanup()

	captured, release := captureSubagentResults(parent)
	defer release()

	child := newChildStub("child-st", 4)
	go parent.consumeChildOutbox(child)

	child.out <- protocol.NewSessionTerminated(child.id, agentParticipant("a1"),
		protocol.SessionTerminatedPayload{
			Reason:    protocol.TerminationHardCeiling,
			Result:    "ceiling at iter 8",
			TurnsUsed: 8,
		})
	close(child.out)

	waitFor(t, func() bool { return captured.len() >= 1 }, 2*time.Second)

	sr := captured.snapshot()[0]
	if sr.Payload.Reason != protocol.TerminationHardCeiling {
		t.Errorf("reason = %q, want %q", sr.Payload.Reason, protocol.TerminationHardCeiling)
	}
	if sr.Payload.Result != "ceiling at iter 8" {
		t.Errorf("result = %q, want %q", sr.Payload.Result, "ceiling at iter 8")
	}
	if sr.Payload.TurnsUsed != 8 {
		t.Errorf("turns = %d, want 8", sr.Payload.TurnsUsed)
	}
}

// TestPump_AbnormalCloseFinalizer verifies the post-loop finalizer:
// when child's outbox closes without any projectable frame at all,
// pump synthesises SubagentResult{Reason:"abnormal_close"} so a
// blocked wait_subagents on the parent unblocks.
func TestPump_AbnormalCloseFinalizer(t *testing.T) {
	parent, cleanup := newTestParent(t, withTestRunLoop())
	defer cleanup()

	captured, release := captureSubagentResults(parent)
	defer release()

	child := newChildStub("child-abnormal", 4)
	go parent.consumeChildOutbox(child)
	close(child.out)

	waitFor(t, func() bool { return captured.len() >= 1 }, 2*time.Second)

	sr := captured.snapshot()[0]
	if sr.Payload.Reason != "abnormal_close" {
		t.Errorf("reason = %q, want abnormal_close", sr.Payload.Reason)
	}
	if sr.Payload.TurnsUsed != 0 {
		t.Errorf("turns = %d, want 0 (no consolidated rows seen)", sr.Payload.TurnsUsed)
	}
}

// TestPump_OnlyOneProjection asserts the projected gate: a child that
// emits Final:true followed by SessionTerminated produces ONE
// SubagentResult, not two. The terminal SessionTerminated drains
// silently because the gate already flipped.
func TestPump_OnlyOneProjection(t *testing.T) {
	parent, cleanup := newTestParent(t, withTestRunLoop())
	defer cleanup()

	captured, release := captureSubagentResults(parent)
	defer release()

	child := newChildStub("child-once", 4)
	go parent.consumeChildOutbox(child)

	child.out <- protocol.NewAgentMessageConsolidated(child.id, agentParticipant("a1"),
		"final answer", 0, true, nil, "", "")
	child.out <- protocol.NewSessionTerminated(child.id, agentParticipant("a1"),
		protocol.SessionTerminatedPayload{Reason: protocol.TerminationCompleted})
	close(child.out)

	// Give the pump time to drain the second frame.
	waitFor(t, func() bool { return captured.len() >= 1 }, 2*time.Second)
	time.Sleep(50 * time.Millisecond) // allow second frame to (be) drain(ed)

	if got := captured.len(); got != 1 {
		t.Errorf("captured = %d (%v), want 1", got, projectionReasons(captured.snapshot()))
	}
}

// TestPump_OfflineParentFallback exercises the IsClosed guard: when
// parent has already terminated, projectToParent skips Submit and
// writes the SubagentResult directly to the events store via
// appendSubagentResultRow. The recovery path's settleDanglingSubagents
// finds it on the next restart.
func TestPump_OfflineParentFallback(t *testing.T) {
	// No runLoop — we want a parent that's IsClosed before the pump
	// fires its projection. MarkClosed flips the flag without going
	// through teardown so the test stays deterministic.
	parent, cleanup := newTestParent(t)
	defer cleanup()
	parent.MarkClosed()

	child := newChildStub("child-offline", 4)
	go parent.consumeChildOutbox(child)

	child.out <- protocol.NewAgentMessageConsolidated(child.id, agentParticipant("a1"),
		"answered before parent died", 0, true, nil, "", "")
	close(child.out)

	// Wait for pump goroutine to land its store write.
	waitFor(t, func() bool {
		rows, err := parent.deps.Store.ListEvents(context.Background(), parent.id,
			store.ListEventsOpts{
				Kinds: []string{string(protocol.KindSubagentResult)},
			})
		return err == nil && len(rows) >= 1
	}, 2*time.Second)

	rows, _ := parent.deps.Store.ListEvents(context.Background(), parent.id,
		store.ListEventsOpts{
			Kinds: []string{string(protocol.KindSubagentResult)},
		})
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 SubagentResult on parent's events", len(rows))
	}
	got, _ := rows[0].Metadata["session_id"].(string)
	if got != child.id {
		t.Errorf("metadata.session_id = %q, want %q", got, child.id)
	}
}

// projectionReasons is a debug helper for failing assertions —
// extracts the per-projection reason for a more readable error.
func projectionReasons(srs []*protocol.SubagentResult) []string {
	out := make([]string, len(srs))
	for i, sr := range srs {
		out[i] = sr.Payload.Reason
	}
	return out
}
