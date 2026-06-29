package a2a

import (
	"context"
	"fmt"
	"iter"
	"log/slog"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// echoExecutor is the A1/A2 placeholder AgentExecutor. As of A2 it resolves
// the contextId → durable root session (opening or resuming as needed) and
// echoes the inbound text back, tagged with the resolved root id — so the
// contextId-session binding is exercised live on every turn. Emitting an
// a2a.Message terminates the A2A turn (the server stops processing events
// after a Message), so a single yield is a complete, spec-correct sync reply.
//
// The real executor — Submit a Frame into the resolved session, subscribe,
// translate the outbound Frame stream — lands in A3–A6 and replaces the echo.
type echoExecutor struct {
	logger *slog.Logger
	reg    *contextRegistry
}

func newEchoExecutor(l *slog.Logger, reg *contextRegistry) *echoExecutor {
	if l == nil {
		l = slog.Default()
	}
	return &echoExecutor{logger: l, reg: reg}
}

// Execute implements a2asrv.AgentExecutor.
func (e *echoExecutor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		cs, err := e.reg.resolve(ctx, execCtx.ContextID)
		if err != nil {
			yield(nil, err)
			return
		}
		text := messageText(execCtx.Message)
		e.logger.Debug("a2a: echo execute",
			"context_id", execCtx.ContextID, "root", cs.RootID(), "task_id", execCtx.TaskID, "len", len(text))
		reply := a2a.NewMessage(a2a.MessageRoleAgent,
			a2a.NewTextPart(fmt.Sprintf("echo[%s]: %s", cs.RootID(), text)))
		yield(reply, nil)
	}
}

// Cancel implements a2asrv.AgentExecutor. The minimal correct response is a
// canceled status update for the task.
func (e *echoExecutor) Cancel(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
	}
}

// messageText concatenates the text content of every TextPart in m. Non-text
// parts (files, structured data) are ignored at A1; multimodal inbound is A10.
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
