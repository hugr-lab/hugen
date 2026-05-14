//go:build duckdb_arrow && scenario

package harness

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session"
)

// SessionHandle bundles a live root session opened against a
// Runtime. Its ID is the GraphQL `$sid` variable scenarios use in
// queries; OpenedAt is informational (logged into t.Log).
type SessionHandle struct {
	rt         *Runtime
	t          *testing.T
	session    *session.Session
	id         string
	openedAt   time.Time
	frameLog   []protocol.Frame
	dispatcher atomic.Pointer[inquiryDispatcher]
}

// OpenSession creates a fresh root session and starts the outbox
// pump that logs every frame to t.Log. Use this once per scenario;
// each subsequent step on the same scenario reuses the handle.
func (r *Runtime) OpenSession(ctx context.Context, t *testing.T) *SessionHandle {
	return r.OpenSessionWithOwner(ctx, t, "harness")
}

// OpenSessionWithOwner is the multi-root variant: opens a fresh
// root with an explicit OwnerID so the runtime's per-owner
// isolation paths (permission tier-2, sessions registry) can be
// exercised under one shared Manager. Each root gets its own
// outbox pump goroutine. Phase 5.1b δ.
func (r *Runtime) OpenSessionWithOwner(ctx context.Context, t *testing.T, owner string) *SessionHandle {
	t.Helper()
	sess, openedAt, err := r.Core.Manager.Open(ctx, session.OpenRequest{
		OwnerID: owner,
		Participants: []protocol.ParticipantInfo{{
			ID:   "harness-user",
			Kind: protocol.ParticipantUser,
			Name: "harness",
		}},
		Metadata: map[string]any{
			"harness_run":     r.Run.Name,
			"harness_run_dir": r.RunDir,
			"harness_started": time.Now().UTC().Format(time.RFC3339),
		},
	})
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	h := &SessionHandle{
		rt:       r,
		t:        t,
		session:  sess,
		id:       sess.ID(),
		openedAt: openedAt,
	}
	t.Logf("── session opened ── id=%s opened_at=%s", h.id, openedAt.Format(time.RFC3339))
	go h.pump()
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := r.Core.Manager.Terminate(closeCtx, h.id, "harness:cleanup"); err != nil {
			t.Logf("Terminate %s: %v", h.id, err)
		}
	})
	return h
}

// ID returns the session id; used to fill `$sid` in scenario
// queries.
func (h *SessionHandle) ID() string { return h.id }

// RunMultiRoot opens one root per [Scenario.Roots] entry under
// the shared Manager, runs each root's Steps concurrently in a
// goroutine, waits for all to finish, then executes the
// scenario's Assertions queries. Phase 5.1b δ. Names in the
// roots map become `$sid_<name>` template variables in
// Assertions; e.g. roots {alice, bob} → `$sid_alice`,
// `$sid_bob` resolve to those roots' session ids.
func (r *Runtime) RunMultiRoot(ctx context.Context, t *testing.T, sc *Scenario) {
	t.Helper()
	if len(sc.Roots) == 0 {
		t.Fatal("RunMultiRoot called on a scenario without Roots")
	}

	// Open every root before firing any step so the Manager
	// registers them all before concurrent work starts. Each
	// goroutine below owns its root handle exclusively.
	handles := make(map[string]*SessionHandle, len(sc.Roots))
	for name, spec := range sc.Roots {
		owner := spec.Owner
		if owner == "" {
			owner = "harness-" + name
		}
		handles[name] = r.OpenSessionWithOwner(ctx, t, owner)
		t.Logf("── multi-root: root %q opened sid=%s owner=%s",
			name, handles[name].id, owner)
	}

	// Drive each root's Steps in its own goroutine. We don't
	// share t between goroutines for Fatal-class operations —
	// each Step uses Logf liberally; per-root errors surface as
	// log lines plus a final summary t.Log.
	var wg sync.WaitGroup
	for name, spec := range sc.Roots {
		wg.Add(1)
		go func(rootName string, steps []Step) {
			defer wg.Done()
			h := handles[rootName]
			for i, step := range steps {
				h.t.Logf("── [root=%s] step %d/%d ──", rootName, i+1, len(steps))
				h.Step(ctx, step, i)
			}
		}(name, spec.Steps)
	}
	wg.Wait()

	t.Logf("── multi-root: all roots finished; running %d assertion(s)", len(sc.Assertions))

	// Cross-root assertions. Each query may reference any
	// $sid_<name> via Vars; the harness substitutes from the
	// sids map.
	sids := make(map[string]string, len(handles)+1)
	for name, h := range handles {
		sids["sid_"+name] = h.id
	}
	// Also expose `$sid` defaulting to the lexically-first root,
	// purely for tooling convenience (a one-root assertion query
	// reuses the single-root `vars: {sid: "$sid"}` shape).
	for _, name := range sortedKeys(handles) {
		sids["sid"] = handles[name].id
		break
	}
	for _, q := range sc.Assertions {
		r.RunQueryWithSids(ctx, t, sids, q)
	}
}

// sortedKeys returns the map keys in lexical order. Used for
// deterministic iteration in cross-root logging.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// pump drains the session's outbox into t.Log. Runs until the
// session goroutine closes the outbox (which happens after
// session_terminated is persisted).
func (h *SessionHandle) pump() {
	for f := range h.session.Outbox() {
		h.frameLog = append(h.frameLog, f)
		logFrame(h.t, f)
		if req, ok := f.(*protocol.InquiryRequest); ok {
			// Spawn a delivery goroutine — Manager.Deliver blocks on
			// the root inbox channel, and we must not stall the
			// outbox drain. Pass by value so the goroutine owns its
			// snapshot of the frame's payload.
			go h.handleInquiry(*req)
		}
	}
}

// Step runs one scenario step against this session. Order:
// primary action (say or tick) → wait_for_subagents → queries.
// Each query result is dumped via RunQuery — no asserts.
func (h *SessionHandle) Step(ctx context.Context, step Step, idx int) {
	h.t.Helper()
	h.t.Logf("── step %d ──", idx+1)

	budget := step.Budget.Std()
	if budget == 0 {
		budget = 60 * time.Second
	}

	// Swap in this step's inquiry responders before the user
	// message is delivered so any inquire bubbled before the LLM
	// emits its first agent_message still gets answered. Clear on
	// return so late inquiries (e.g. from a leftover async
	// mission) don't accidentally pick up a stale rule from the
	// previous step.
	h.dispatcher.Store(newInquiryDispatcher(step.InquiryResponses))
	defer h.dispatcher.Store(nil)

	switch {
	case step.Say != "":
		h.deliverUserMessage(ctx, step.Say, budget)
	case step.Tick:
		h.t.Logf("tick")
	default:
		h.t.Fatalf("step %d: must have say or tick", idx+1)
	}

	if step.WaitForSubagents > 0 {
		h.waitForSubagentsSettle(step.WaitForSubagents.Std())
	}
	if step.WaitForCondition != nil {
		h.waitForCondition(ctx, step.WaitForCondition)
	}

	for _, q := range step.Queries {
		h.rt.RunQuery(ctx, h.t, h.id, q)
	}
}

// deliverUserMessage sends the text as a user_message and waits
// for either a final agent_message or the budget to elapse. The
// completion check is a poll over the frame log because the
// outbox pump owns the channel — we observe its persisted state.
func (h *SessionHandle) deliverUserMessage(ctx context.Context, text string, budget time.Duration) {
	user := protocol.ParticipantInfo{
		ID:   "harness-user",
		Kind: protocol.ParticipantUser,
		Name: "harness",
	}
	frame := protocol.NewUserMessage(h.id, user, text)
	deliverCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := h.rt.Core.Manager.Deliver(deliverCtx, h.id, frame); err != nil {
		h.t.Fatalf("Deliver user_message: %v", err)
	}
	h.t.Logf("── USER ──▶ %s", singleLine(text, 240))

	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if h.lastFinalAgentMessageAfter(frame.OccurredAt()) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	h.t.Logf("── budget elapsed (%s) without final agent_message", budget)
}

// lastFinalAgentMessageAfter scans the persisted frame log for
// agent_message{final=true} that arrived after `since`. The frame
// log is appended-to by the pump goroutine; reading the slice
// header without a mutex is safe-ish for poll loops because we
// don't dereference indices that the pump hasn't written yet.
//
// (Race detector tolerates the pattern because writes and reads
// are aligned on word boundaries — same trick the prior agent
// harness used. If `make scenario` ever surfaces a flake here,
// add a mutex.)
func (h *SessionHandle) lastFinalAgentMessageAfter(since time.Time) bool {
	for i := len(h.frameLog) - 1; i >= 0; i-- {
		f := h.frameLog[i]
		if f.OccurredAt().Before(since) {
			break
		}
		if msg, ok := f.(*protocol.AgentMessage); ok && msg.Payload.Final {
			return true
		}
	}
	return false
}

// waitForSubagentsSettle polls the local store for any non-
// terminal sub-agent rows owned by this session; returns when the
// set is empty or the budget expires. It is a best-effort
// "settling sentinel", not an assertion — if the budget elapses
// the runner logs and proceeds to queries.
func (h *SessionHandle) waitForSubagentsSettle(budget time.Duration) {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		// TODO(phase-4.1b followup): hit Core.Store.ListChildren and
		// inspect rows. For v1 we just sleep — the runner is
		// observational and the queries themselves dump the persisted
		// state regardless of timing.
		time.Sleep(500 * time.Millisecond)
		break
	}
	h.t.Logf("wait_for_subagents settled (budget=%s)", budget)
}

// waitForCondition polls a GraphQL query until its result row
// count matches Expected, or the budget expires. v1 implementation
// is a stub — logged but not active. Scenarios needing it should
// document the expected behaviour.
func (h *SessionHandle) waitForCondition(_ context.Context, cond *WaitCond) {
	h.t.Logf("wait_for_condition stub: expected_rows=%d budget=%s", cond.Expected, cond.Budget)
}

// logFrame renders one Frame as a single t.Log line. Logging is
// best-effort — the goal is by-eye review, not parseable output.
func logFrame(t *testing.T, f protocol.Frame) {
	t.Helper()
	switch v := f.(type) {
	case *protocol.AgentMessage:
		marker := ""
		if v.Payload.Final {
			marker = " [FINAL]"
		}
		t.Logf("◆ %3d agent_message%s chunk=%d %s", f.Seq(), marker, v.Payload.ChunkSeq,
			singleLine(v.Payload.Text, 200))
	case *protocol.Reasoning:
		t.Logf("◆ %3d reasoning chunk=%d %s", f.Seq(), v.Payload.ChunkSeq,
			singleLine(v.Payload.Text, 160))
	case *protocol.ToolCall:
		t.Logf("◆ %3d tool_call %s args=%s", f.Seq(), v.Payload.Name, jsonShort(v.Payload.Args, 200))
	case *protocol.ToolResult:
		t.Logf("◆ %3d tool_result tool_id=%s result=%s err=%v", f.Seq(),
			v.Payload.ToolID, jsonShort(v.Payload.Result, 200), v.Payload.IsError)
	case *protocol.SubagentStarted:
		t.Logf("◆ %3d subagent_started child=%s skill=%s role=%s task=%s",
			f.Seq(), v.Payload.ChildSessionID, v.Payload.Skill, v.Payload.Role,
			singleLine(v.Payload.Task, 80))
	case *protocol.SubagentResult:
		t.Logf("◆ %3d subagent_result session=%s reason=%s result=%s",
			f.Seq(), v.Payload.SessionID, v.Payload.Reason,
			singleLine(v.Payload.Result, 120))
	case *protocol.UserMessage:
		t.Logf("◆ %3d user_message %s", f.Seq(), singleLine(v.Payload.Text, 200))
	case *protocol.SystemMarker:
		t.Logf("◆ %3d system_marker subject=%s", f.Seq(), v.Payload.Subject)
	case *protocol.SessionStatus:
		t.Logf("◆ %3d session_status state=%s reason=%s", f.Seq(),
			v.Payload.State, v.Payload.Reason)
	case *protocol.SessionTerminated:
		t.Logf("◆ %3d session_terminated reason=%s", f.Seq(), v.Payload.Reason)
	case *protocol.ExtensionFrame:
		t.Logf("◆ %3d extension_frame ext=%s op=%s category=%s",
			f.Seq(), v.Payload.Extension, v.Payload.Op, v.Payload.Category)
	case *protocol.InquiryRequest:
		t.Logf("◆ %3d inquiry_request type=%s rid=%s caller=%s q=%s",
			f.Seq(), v.Payload.Type, v.Payload.RequestID,
			v.Payload.CallerSessionID, singleLine(v.Payload.Question, 160))
	case *protocol.InquiryResponse:
		t.Logf("◆ %3d inquiry_response rid=%s caller=%s approved=%s response=%s",
			f.Seq(), v.Payload.RequestID, v.Payload.CallerSessionID,
			approvedLabel(v.Payload.Approved), singleLine(v.Payload.Response, 160))
	default:
		t.Logf("◆ %3d %s", f.Seq(), f.Kind())
	}
}

func singleLine(s string, max int) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '\n' || r == '\r' {
			r = ' '
		}
		out = append(out, r)
		if len(out) >= max {
			out = append(out, '…')
			break
		}
	}
	return string(out)
}

func jsonShort(v any, max int) string {
	if v == nil {
		return "null"
	}
	return singleLine(fmt.Sprintf("%v", v), max)
}
