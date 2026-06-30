package a2a

import (
	"context"
	"fmt"
	"iter"
	"log/slog"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// reasonSessionOpened is the SessionStatus reason the runtime stamps on the
// idle marker emitted when a session first opens — emitted BEFORE any turn, so
// the executor must not mistake it for a turn boundary.
const reasonSessionOpened = "session_opened"

// chatHistoryMetaKey is the Copilot Studio metadata key carrying the full,
// replayed conversation on every inbound (verified, a2a-integration.md §2.3a).
// We normally ignore it (the durable session has its own history); it is only
// consulted as a fallback when message.parts is empty.
const chatHistoryMetaKey = "copilotstudio.microsoft.com/a2a/chathistory"

// frameIO is the narrow inbound/outbound Frame surface the executor needs:
// submit a frame into a session and subscribe to its outbox. adapter.Host
// satisfies it; keeping the dependency narrow (no *session.Session) makes the
// drain/translate logic unit-testable with a fake channel — the same seam
// discipline as A2's rootStore.
type frameIO interface {
	Submit(ctx context.Context, f protocol.Frame) error
	Subscribe(ctx context.Context, sessionID string) (<-chan protocol.Frame, error)
}

// sessionExecutor is the A3 AgentExecutor: it resolves the contextId to a
// durable root session (A2), submits the inbound message as a user turn, and
// translates the session's outbound Frame stream into A2A events — inline, in
// the SDK-provided per-invocation goroutine. No dedicated reader goroutine:
// a synchronous turn is fully drained within Execute. Async missions (frames
// that outlive the turn) get a long-lived reader in A6; parked-inquiry state
// (A5) lives on the ContextSession, not a goroutine.
type sessionExecutor struct {
	logger *slog.Logger
	reg    *contextRegistry
	io     frameIO
	owner  protocol.ParticipantInfo
}

func newSessionExecutor(l *slog.Logger, reg *contextRegistry, io frameIO, owner protocol.ParticipantInfo) *sessionExecutor {
	if l == nil {
		l = slog.Default()
	}
	return &sessionExecutor{logger: l, reg: reg, io: io, owner: owner}
}

// Execute implements a2asrv.AgentExecutor. Sync-turn path (A3): one inbound
// user message → one agent reply. Subscribe-before-submit so the turn's first
// frames aren't missed; drain until the idle (turn_complete) boundary.
func (e *sessionExecutor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		cs, err := e.reg.resolve(execCtx.ContextID)
		if err != nil {
			yield(nil, err)
			return
		}
		rootID := cs.RootID()

		// Turn-scoped subscription: cancel on return drops it from the
		// runtime's subscriber set (the runtime owns the channel close).
		turnCtx, cancel := context.WithCancel(ctx)
		defer cancel()

		sub, err := e.io.Subscribe(turnCtx, rootID)
		if err != nil {
			yield(nil, fmt.Errorf("a2a: subscribe %s: %w", rootID, err))
			return
		}

		// The new user turn is in message.parts (A4 / §2.3a verified). The
		// Copilot chathistory metadata is the full, duplicated conversation —
		// ignored here because our durable session already holds the history;
		// feeding it back would double it. Defensive fallback only: if parts is
		// empty, recover the latest user turn from the chathistory tail.
		text := messageText(execCtx.Message)
		if text == "" {
			if recovered := latestUserTextFromHistory(execCtx.Message); recovered != "" {
				e.logger.Debug("a2a: empty parts; recovered text from chathistory", "context_id", execCtx.ContextID)
				text = recovered
			}
		}

		// taskBorn tracks whether an A2A Task has been materialised for THIS
		// Execute — either the client continued one (StoredTask != nil) or we
		// emitted a Task/working/input-required event below. Once a Task exists
		// the SDK forbids a bare Message to finish (taskupdate manager), so the
		// terminal/working helpers branch on it. Threaded by pointer through the
		// drain so a `working` emitted mid-drain (A6) flips the finish path.
		taskBorn := execCtx.StoredTask != nil

		// awaited is THIS Execute's set of async sub-agent session ids it is
		// holding the Task open for (A6) — local to this turn so concurrent
		// Tasks on one context each finish on their own async work. Restored
		// from a parked inquiry when the inquiry fired inside a running async
		// mission (so the Execute that answers resumes the hold).
		awaited := map[string]struct{}{}

		// A5: if this context is parked on an inquiry, the inbound text is the
		// ANSWER — route it down as an InquiryResponse (cascade-down), not a new
		// user turn. The park is keyed on the contextId, so it resolves even when
		// a client replays with a fresh task (Copilot) rather than honouring
		// stateful input-required.
		if pend := cs.peekPending(); pend != nil {
			resp, err := buildInquiryResponse(e.owner, rootID, pend, text)
			if err != nil {
				// Unparseable answer (e.g. an empty approval). Keep the inquiry
				// parked and re-ask — the session is still blocked in
				// session:inquire, so we must not submit a user turn.
				e.logger.Debug("a2a: inquiry answer unparseable; re-asking",
					"context_id", execCtx.ContextID, "err", err)
				e.requestInput(execCtx, &taskBorn, fmt.Sprintf("%s\n\n(%s)", pend.Question, err), yield)
				return
			}
			cs.clearPending()
			if err := e.io.Submit(turnCtx, resp); err != nil {
				yield(nil, fmt.Errorf("a2a: submit inquiry_response to %s: %w", rootID, err))
				return
			}
			e.logger.Debug("a2a: inquiry answered",
				"context_id", execCtx.ContextID, "root", rootID, "kind", pend.Kind)
			// A6: the inquiry fired inside a running async mission — resume
			// holding for the work it was waiting on and tell the client the
			// answer was accepted and the Task is still working.
			for _, id := range pend.AsyncAwaited {
				awaited[id] = struct{}{}
			}
			if len(awaited) > 0 {
				e.reportWorking(execCtx, &taskBorn, "", len(awaited), yield)
			}
		} else {
			um := protocol.NewUserMessage(rootID, e.owner, text)
			if err := e.io.Submit(turnCtx, um); err != nil {
				yield(nil, fmt.Errorf("a2a: submit user_message to %s: %w", rootID, err))
				return
			}
			e.logger.Debug("a2a: turn submitted", "context_id", execCtx.ContextID, "root", rootID, "len", len(text))
		}

		e.drainTurn(turnCtx, sub, execCtx, cs, &taskBorn, awaited, yield)
	}
}

// drainTurn reads the session's outbox until this Execute's Task reaches a
// boundary. The turn boundary is the `AgentMessage{Consolidated, Final}` frame
// (the model retired the turn) — NOT idle, because the runtime keeps root
// active while an async sub-agent runs and never emits idle until it is fully
// quiescent. On each Final:
//   - new async sub-agents this turn launched (ActiveAsync diffed against the
//     context's known set) join `awaited` → the Task stays `working`;
//   - sub-agents whose result this turn surfaces (ResultOf ∩ awaited) leave it;
//   - when `awaited` is empty the Task is done → finish.
//
// An InquiryRequest parks the Task as input-required (A5), stashing `awaited`
// so the Execute that answers resumes the hold. idle is a fallback boundary for
// an empty turn that emitted no Final frame.
func (e *sessionExecutor) drainTurn(
	ctx context.Context,
	sub <-chan protocol.Frame,
	execCtx *a2asrv.ExecutorContext,
	cs *contextSession,
	taskBorn *bool,
	awaited map[string]struct{},
	yield func(a2a.Event, error) bool,
) {
	var b strings.Builder
	for {
		select {
		case <-ctx.Done():
			// Client gave up or shutdown — emit what we have so the turn
			// isn't left dangling, then stop. The session keeps running;
			// its tail frames persist in the event log.
			e.finishTurn(execCtx, *taskBorn, b.String(), yield)
			return
		case f, ok := <-sub:
			if !ok {
				e.finishTurn(execCtx, *taskBorn, b.String(), yield)
				return
			}
			switch fr := f.(type) {
			case *protocol.AgentMessage:
				if !fr.Payload.Consolidated {
					// LIVE streaming chunk — accumulate the incremental text (the
					// TUI assembles the same way). The Consolidated row duplicates
					// it, so only chunks feed b.
					b.WriteString(fr.Payload.Text)
					continue
				}
				if !fr.Payload.Final {
					// Per-iteration record of a tool-calling iteration; the live
					// chunks already carried its text. Not a turn boundary.
					continue
				}
				// Turn boundary. Attribute async work (A6).
				fresh := cs.recordNewAsync(fr.Payload.ActiveAsync)
				for _, id := range fresh {
					awaited[id] = struct{}{} // this turn launched it → this Task awaits it
				}
				matched := e.collectResults(cs, awaited, fr.Payload.ResultOf)
				if len(awaited) == 0 {
					// No async pending for this Task — a plain turn, or the last
					// thing it awaited just completed. Deliver the reply.
					e.logger.Debug("a2a: turn complete",
						"context_id", execCtx.ContextID, "len", b.Len())
					e.finishTurn(execCtx, *taskBorn, b.String(), yield)
					return
				}
				// Still holding. Report progress only when THIS frame moved this
				// Task's work — it launched async (the ack) or one of its awaited
				// results landed. Another Task's summary on the shared outbox is
				// not ours to surface.
				if len(fresh) > 0 || matched {
					e.reportWorking(execCtx, taskBorn, b.String(), len(awaited), yield)
				}
				b.Reset()
			case *protocol.InquiryRequest:
				// A5: a tier called session:inquire — surface it as input-required
				// and park. An inquiry raised inside a running async mission flips
				// the held Task to input-required; `awaited` is stashed so the
				// Execute that answers resumes the hold (taskBorn respected — no
				// double-materialise).
				e.parkAndRequestInput(cs, execCtx, &fr.Payload, b.String(), taskBorn, awaitedIDs(awaited), yield)
				return
			case *protocol.Error:
				yield(nil, fmt.Errorf("a2a: session error [%s]: %s", fr.Payload.Code, fr.Payload.Message))
				return
			case *protocol.SessionStatus:
				if fr.Payload.State != protocol.SessionStatusIdle ||
					fr.Payload.Reason == reasonSessionOpened {
					continue
				}
				// idle (turn_complete / cancelled / stream_error) with no async
				// pending — a fallback boundary for an empty turn that emitted no
				// Final AgentMessage (the model produced nothing). While holding
				// (awaited non-empty) the session is not quiescent, so this
				// shouldn't fire; if it does, keep holding for the result.
				if len(awaited) == 0 {
					e.finishTurn(execCtx, *taskBorn, b.String(), yield)
					return
				}
			}
		}
	}
}

// collectResults removes from awaited every async sub-agent whose result the
// current Final frame surfaces (ResultOf ∩ awaited) and forgets them from the
// context's global set. Returns true when at least one was this Task's — the
// signal that this frame is worth surfacing as progress. Phase 8/A6.
func (e *sessionExecutor) collectResults(cs *contextSession, awaited map[string]struct{}, resultOf []protocol.ActiveSubagentRef) bool {
	var done []string
	for _, r := range resultOf {
		if _, ok := awaited[r.SessionID]; ok {
			delete(awaited, r.SessionID)
			done = append(done, r.SessionID)
		}
	}
	if len(done) == 0 {
		return false
	}
	cs.forgetAsync(done)
	return true
}

// awaitedIDs returns the awaited set as a slice for stashing on a parked
// inquiry. Order is irrelevant (rebuilt into a set on resume).
func awaitedIDs(awaited map[string]struct{}) []string {
	if len(awaited) == 0 {
		return nil
	}
	out := make([]string, 0, len(awaited))
	for id := range awaited {
		out = append(out, id)
	}
	return out
}

// finishTurn yields the terminal event carrying the assistant reply. On a plain
// turn (no task materialised) that is a bare agent Message. But once a Task
// exists — the client continued one (StoredTask != nil) OR we emitted a
// working/input-required event this turn (taskBorn) — the SDK rejects a bare
// Message after a task is stored, so we complete via a status update carrying
// the reply as the status message.
func (e *sessionExecutor) finishTurn(execCtx *a2asrv.ExecutorContext, taskBorn bool, text string, yield func(a2a.Event, error) bool) {
	msg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(text))
	if taskBorn {
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCompleted, msg), nil)
		return
	}
	yield(msg, nil)
}

// reportWorking emits a `working` status update for an in-flight async mission
// (A6), materialising the Task first if this turn hasn't yet (a fresh turn that
// went async with no prior Task). text — the interim/ack text streamed so far —
// rides the status message; an empty text yields a status with no message.
// Streaming clients see these live; a non-streaming client gets only the final
// Task, but the working event still drives the Task state in the store.
func (e *sessionExecutor) reportWorking(execCtx *a2asrv.ExecutorContext, taskBorn *bool, text string, activeCount int, yield func(a2a.Event, error) bool) {
	if !*taskBorn {
		if !yield(a2a.NewSubmittedTask(execCtx, execCtx.Message), nil) {
			return
		}
		*taskBorn = true
	}
	var msg *a2a.Message
	if strings.TrimSpace(text) != "" {
		msg = a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(text))
	}
	e.logger.Debug("a2a: async mission in flight; task working",
		"context_id", execCtx.ContextID, "active_subagents", activeCount)
	yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateWorking, msg), nil)
}

// parkAndRequestInput records the in-flight inquiry on the context session and
// surfaces it to the client as an input-required task. Spec §A5.
func (e *sessionExecutor) parkAndRequestInput(
	cs *contextSession,
	execCtx *a2asrv.ExecutorContext,
	p *protocol.InquiryRequestPayload,
	preamble string,
	taskBorn *bool,
	asyncAwaited []string,
	yield func(a2a.Event, error) bool,
) {
	prompt := inquiryPrompt(p, preamble)
	cs.park(&parkedInquiry{
		RequestID:       p.RequestID,
		CallerSessionID: p.CallerSessionID,
		Kind:            p.Type,
		Question:        prompt,
		AsyncAwaited:    asyncAwaited,
	})
	e.logger.Debug("a2a: parking inquiry as input-required",
		"context_id", execCtx.ContextID, "kind", p.Type, "request_id", p.RequestID,
		"async_awaited", len(asyncAwaited))
	e.requestInput(execCtx, taskBorn, prompt, yield)
}

// requestInput emits the input-required task carrying prompt. If no Task exists
// yet (taskBorn false) it first materialises one with a submitted event — the
// SDK requires the first event on a new task to be a Task, not a status update
// (taskupdate manager). When a Task already exists (a continuation, or a held
// async long-task that an inner inquiry flips to input-required) the status
// update goes out alone.
func (e *sessionExecutor) requestInput(execCtx *a2asrv.ExecutorContext, taskBorn *bool, prompt string, yield func(a2a.Event, error) bool) {
	if !*taskBorn {
		if !yield(a2a.NewSubmittedTask(execCtx, execCtx.Message), nil) {
			return
		}
		*taskBorn = true
	}
	msg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(prompt))
	yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateInputRequired, msg), nil)
}

// Cancel implements a2asrv.AgentExecutor. The minimal correct response is a
// canceled status update for the task. A6 cascades a real session Cancel.
func (e *sessionExecutor) Cancel(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
	}
}

// latestUserTextFromHistory recovers the most recent user utterance from the
// Copilot chathistory metadata envelope — the A4 fallback for an inbound whose
// message.parts is empty. Shape (verified, §2.3a):
//
//	metadata["…/chathistory"] = [ {HasValue, Value:[ {From, Text, …}, … ]} ]
//
// User entries carry From:"" (agent entries carry an internal name), so the
// last entry with From=="" is the current user turn. Returns "" if absent.
func latestUserTextFromHistory(m *a2a.Message) string {
	if m == nil || m.Metadata == nil {
		return ""
	}
	outer, ok := m.Metadata[chatHistoryMetaKey].([]any)
	if !ok {
		return ""
	}
	var last string
	for _, o := range outer {
		om, ok := o.(map[string]any)
		if !ok {
			continue
		}
		vals, ok := om["Value"].([]any)
		if !ok {
			continue
		}
		for _, v := range vals {
			vm, ok := v.(map[string]any)
			if !ok {
				continue
			}
			from, _ := vm["From"].(string)
			text, _ := vm["Text"].(string)
			if from == "" && strings.TrimSpace(text) != "" {
				last = text
			}
		}
	}
	return strings.TrimSpace(last)
}

// messageText concatenates the text content of every TextPart in m. Non-text
// parts (files, structured data) are ignored until A10 (multimodal inbound).
func messageText(m *a2a.Message) string {
	if m == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range m.Parts {
		if t := p.Text(); t != "" {
			b.WriteString(t)
		}
	}
	return b.String()
}
