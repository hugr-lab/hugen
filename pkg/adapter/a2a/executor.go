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

		text := messageText(execCtx.Message)
		um := protocol.NewUserMessage(rootID, e.owner, text)
		if err := e.io.Submit(turnCtx, um); err != nil {
			yield(nil, fmt.Errorf("a2a: submit user_message to %s: %w", rootID, err))
			return
		}
		e.logger.Debug("a2a: turn submitted", "context_id", execCtx.ContextID, "root", rootID, "len", len(text))

		e.drainTurn(turnCtx, sub, execCtx, yield)
	}
}

// drainTurn reads the session's outbox until the synchronous turn completes,
// accumulating the assistant text and yielding the final A2A Message. It is the
// sync subset of the §4 translation table; A5 (inquiry) and A6 (async mission)
// extend the SessionStatus handling.
func (e *sessionExecutor) drainTurn(
	ctx context.Context,
	sub <-chan protocol.Frame,
	execCtx *a2asrv.ExecutorContext,
	yield func(a2a.Event, error) bool,
) {
	var b strings.Builder
	for {
		select {
		case <-ctx.Done():
			// Client gave up or shutdown — emit what we have so the turn
			// isn't left dangling, then stop. The session keeps running;
			// its tail frames persist in the event log.
			yield(e.replyMessage(b.String()), nil)
			return
		case f, ok := <-sub:
			if !ok {
				yield(e.replyMessage(b.String()), nil)
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
					yield(e.replyMessage(b.String()), nil)
					return
				default:
					// active / wait_subagents (A6) / wait_user_input (A5) /
					// wait_approval — not yet special-cased; keep draining.
				}
			}
		}
	}
}

// replyMessage builds the terminal agent Message for a turn.
func (e *sessionExecutor) replyMessage(text string) a2a.Event {
	return a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(text))
}

// Cancel implements a2asrv.AgentExecutor. The minimal correct response is a
// canceled status update for the task. A6 cascades a real session Cancel.
func (e *sessionExecutor) Cancel(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
	}
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
