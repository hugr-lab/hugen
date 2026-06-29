package a2a

import (
	"context"
	"iter"
	"log/slog"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// echoExecutor is the A1 placeholder AgentExecutor: it echoes the inbound
// text straight back as an agent Message. Emitting an a2a.Message
// terminates the A2A turn (the server stops processing events after a
// Message), so a single yield is a complete, spec-correct sync response.
//
// The real executor — resolve the contextId session, Submit a Frame,
// translate the outbound Frame stream — lands in A2–A6 and replaces this.
type echoExecutor struct {
	logger *slog.Logger
}

func newEchoExecutor(l *slog.Logger) *echoExecutor {
	if l == nil {
		l = slog.Default()
	}
	return &echoExecutor{logger: l}
}

// Execute implements a2asrv.AgentExecutor.
func (e *echoExecutor) Execute(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		text := messageText(execCtx.Message)
		e.logger.Debug("a2a: echo execute", "context_id", execCtx.ContextID, "task_id", execCtx.TaskID, "len", len(text))
		reply := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("echo: "+text))
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
