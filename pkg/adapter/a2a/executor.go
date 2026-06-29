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
				e.requestInput(execCtx, fmt.Sprintf("%s\n\n(%s)", pend.Question, err), yield)
				return
			}
			cs.clearPending()
			if err := e.io.Submit(turnCtx, resp); err != nil {
				yield(nil, fmt.Errorf("a2a: submit inquiry_response to %s: %w", rootID, err))
				return
			}
			e.logger.Debug("a2a: inquiry answered",
				"context_id", execCtx.ContextID, "root", rootID, "kind", pend.Kind)
		} else {
			um := protocol.NewUserMessage(rootID, e.owner, text)
			if err := e.io.Submit(turnCtx, um); err != nil {
				yield(nil, fmt.Errorf("a2a: submit user_message to %s: %w", rootID, err))
				return
			}
			e.logger.Debug("a2a: turn submitted", "context_id", execCtx.ContextID, "root", rootID, "len", len(text))
		}

		e.drainTurn(turnCtx, sub, execCtx, cs, yield)
	}
}

// drainTurn reads the session's outbox until the turn reaches a boundary,
// accumulating the assistant text. A turn ends one of three ways: the session
// goes idle (turn_complete → finishTurn), a tier calls session:inquire (an
// InquiryRequest → park as input-required, A5), or it errors. A6 (async
// mission) extends the SessionStatus handling further.
func (e *sessionExecutor) drainTurn(
	ctx context.Context,
	sub <-chan protocol.Frame,
	execCtx *a2asrv.ExecutorContext,
	cs *contextSession,
	yield func(a2a.Event, error) bool,
) {
	var b strings.Builder
	for {
		select {
		case <-ctx.Done():
			// Client gave up or shutdown — emit what we have so the turn
			// isn't left dangling, then stop. The session keeps running;
			// its tail frames persist in the event log.
			e.finishTurn(execCtx, b.String(), yield)
			return
		case f, ok := <-sub:
			if !ok {
				e.finishTurn(execCtx, b.String(), yield)
				return
			}
			switch fr := f.(type) {
			case *protocol.AgentMessage:
				// Accumulate the LIVE streaming chunks (Consolidated=false) —
				// they carry the incremental assistant text on the outbox (the
				// TUI assembles the same way). The Consolidated=true row
				// duplicates that text and is the persist record / finalize
				// signal, so we skip it to avoid double-counting.
				if !fr.Payload.Consolidated {
					b.WriteString(fr.Payload.Text)
				}
			case *protocol.InquiryRequest:
				// A5: a tier called session:inquire — surface it as an A2A
				// input-required task and park. Any assistant text streamed
				// before the question becomes the prompt's preamble. The
				// InquiryRequest (not the SessionStatus(wait_*) frame) is the
				// authoritative park trigger — it always reaches root, the same
				// signal the TUI keys on.
				e.parkAndRequestInput(cs, execCtx, &fr.Payload, b.String(), yield)
				return
			case *protocol.Error:
				yield(nil, fmt.Errorf("a2a: session error [%s]: %s", fr.Payload.Code, fr.Payload.Message))
				return
			case *protocol.SessionStatus:
				switch fr.Payload.State {
				case protocol.SessionStatusIdle:
					// A freshly-opened session emits idle(session_opened) BEFORE
					// our turn even starts — that is not a turn boundary. Only a
					// post-turn idle (turn_complete / cancelled / stream_error)
					// ends the turn.
					if fr.Payload.Reason == reasonSessionOpened {
						continue
					}
					e.logger.Debug("a2a: turn complete",
						"context_id", execCtx.ContextID, "reason", fr.Payload.Reason, "len", b.Len())
					e.finishTurn(execCtx, b.String(), yield)
					return
				default:
					// active / wait_subagents (A6) / wait_user_input /
					// wait_approval — narration only; the InquiryRequest frame
					// above is what actually parks the turn.
				}
			}
		}
	}
}

// finishTurn yields the terminal event carrying the assistant reply. On a plain
// turn (no task materialised) that is a bare agent Message. But once a prior
// turn parked this context as input-required, a Task exists in the store, and
// the SDK rejects a bare Message after a task is stored — so a turn that
// CONTINUES such a task (StoredTask != nil) must complete via a status update
// instead, carrying the reply as the status message.
func (e *sessionExecutor) finishTurn(execCtx *a2asrv.ExecutorContext, text string, yield func(a2a.Event, error) bool) {
	msg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(text))
	if execCtx.StoredTask != nil {
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCompleted, msg), nil)
		return
	}
	yield(msg, nil)
}

// parkAndRequestInput records the in-flight inquiry on the context session and
// surfaces it to the client as an input-required task. Spec §A5.
func (e *sessionExecutor) parkAndRequestInput(
	cs *contextSession,
	execCtx *a2asrv.ExecutorContext,
	p *protocol.InquiryRequestPayload,
	preamble string,
	yield func(a2a.Event, error) bool,
) {
	prompt := inquiryPrompt(p, preamble)
	cs.park(&parkedInquiry{
		RequestID:       p.RequestID,
		CallerSessionID: p.CallerSessionID,
		Kind:            p.Type,
		Question:        prompt,
	})
	e.logger.Debug("a2a: parking inquiry as input-required",
		"context_id", execCtx.ContextID, "kind", p.Type, "request_id", p.RequestID)
	e.requestInput(execCtx, prompt, yield)
}

// requestInput emits the input-required task carrying prompt. On a brand-new
// turn (no StoredTask) it first materialises the task with a submitted event —
// the SDK requires the first event on a new task to be a Task, not a status
// update (taskupdate manager). On a continuation the task already exists, so the
// status update goes out alone.
func (e *sessionExecutor) requestInput(execCtx *a2asrv.ExecutorContext, prompt string, yield func(a2a.Event, error) bool) {
	if execCtx.StoredTask == nil {
		if !yield(a2a.NewSubmittedTask(execCtx, execCtx.Message), nil) {
			return
		}
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
