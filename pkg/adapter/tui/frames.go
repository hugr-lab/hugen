package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// frameMsg wraps a runtime frame for delivery into the Bubble Tea
// program via Program.Send.
type frameMsg struct{ frame protocol.Frame }

// errMsg surfaces an asynchronous error (submit failure, glamour
// render error, etc.) to the model so it can show it in the footer.
type errMsg struct{ err error }

// _ = tea.Msg(frameMsg{}) // type assertion: frameMsg implements tea.Msg
var (
	_ tea.Msg = frameMsg{}
	_ tea.Msg = errMsg{}
)

// handleFrame routes one incoming frame to the appropriate model
// projection. Slice 1 handles the chat-relevant kinds; later slices
// add liveview/inquiry/subagent routing. Returns a tea.Cmd when the
// frame triggers a Bubble Tea action (e.g. SessionClosed → quit
// during shutdown).
func (m *model) handleFrame(f protocol.Frame) tea.Cmd {
	// Auto-flush in-flight reasoning when a non-reasoning frame
	// arrives. Some model adapters never emit Reasoning{Final:true}
	// at end-of-stream; without this guard the pendingReasoning
	// accumulator persists across turns and the chat shows last
	// turn's "thinking…" at the bottom forever.
	if _, isReasoning := f.(*protocol.Reasoning); !isReasoning {
		if m.chat.pendingReasoning.Len() > 0 {
			m.chat.finalizeReasoning()
		}
	}
	switch v := f.(type) {
	case *protocol.UserMessage:
		// Already echoed via appendUserBubble on submit; the
		// runtime's persisted copy is silent here.
		return nil
	case *protocol.AgentMessage:
		m.handleAgentMessage(v)
	case *protocol.Reasoning:
		m.handleReasoning(v)
	case *protocol.ToolCall:
		m.statusLine = fmt.Sprintf("tool: %s", v.Payload.Name)
	case *protocol.ToolResult:
		m.statusLine = "thinking…"
	case *protocol.Error:
		m.bannerError = fmt.Sprintf("%s: %s", v.Payload.Code, v.Payload.Message)
		m.statusLine = "ready"
	case *protocol.SystemMarker:
		m.chat.appendSystem(v.Payload.Subject)
		m.refreshChat()
	case *protocol.ExtensionFrame:
		// Slice 2 — only the liveview status frame populates the
		// sidebar. Other extensions' frames (plan:set,
		// whiteboard:post, …) are intentionally ignored at this
		// layer because the liveview projection already folds them
		// into its payload.
		if v.Payload.Extension == "liveview" && v.Payload.Op == "status" {
			if st, err := parseLiveviewStatus(v.Payload.Data); err == nil {
				m.sidebarStatus = st
			}
		}
	case *protocol.InquiryRequest:
		// Slice 3 — HITL modal. Replace any prior pending inquiry
		// (multiple bubbled inquiries are rare but supported by the
		// runtime; only one fits the modal at a time — the latest
		// wins. Earlier inquiry remains open server-side and will
		// re-prompt on its own timeout).
		m.pendingInquiry = newInquiryState(v)
		m.statusLine = fmt.Sprintf("HITL: %s", v.Payload.Type)
		// Focus the textarea up-front for clarifications (the only
		// input path) and keep it ready for approvals' "reply with
		// reason" mode.
		m.textarea.Reset()
		m.textarea.Focus()
	case *protocol.InquiryResponse:
		// Echo of the operator's own reply (or one synthesised by
		// the runtime on timeout / another adapter). Clear the modal
		// either way — the inquiry is no longer pending.
		m.pendingInquiry = nil
		m.statusLine = "ready"
	case *protocol.SessionTerminated:
		m.chat.appendSystem(fmt.Sprintf("session terminated (%s)", v.Payload.Reason))
		m.refreshChat()
		// Clear any stale inquiry — the runtime is gone.
		m.pendingInquiry = nil
	case *protocol.SessionClosed:
		// Runtime acknowledges close. If the user initiated the
		// shutdown (closing == true), quit the program now —
		// adapter.Run will wait on session.Done() for clean
		// teardown. Otherwise the close came from the runtime side
		// (cancel cascade, etc.); leave the program up so the user
		// can read the transcript and quit themselves.
		m.statusLine = "closed"
		if m.closing {
			return tea.Quit
		}
	}
	return nil
}

func (m *model) handleAgentMessage(v *protocol.AgentMessage) {
	// Render rule mirrors console.adapter.render:
	//   - Consolidated && Final → turn boundary; status back to ready.
	//   - Consolidated && !Final → tool-iteration marker, silent.
	//   - !Consolidated chunks → streaming text appended to current bubble.
	if v.Payload.Consolidated {
		if v.Payload.Final {
			m.chat.finalizeAssistant()
			m.refreshChat()
			m.statusLine = "ready"
		}
		return
	}
	m.chat.appendAssistantChunk(v.Payload.Text)
	m.statusLine = "thinking…"
	m.refreshChat()
}

func (m *model) handleReasoning(v *protocol.Reasoning) {
	if v.Payload.Text == "" {
		return
	}
	m.chat.appendReasoningChunk(v.Payload.Text)
	if v.Payload.Final {
		m.chat.finalizeReasoning()
	}
	m.refreshChat()
}

// chatBuffer is the running transcript backing the viewport. Slice 1
// keeps a simple line-oriented model: each user / assistant /
// reasoning / system block is a span; render concatenates them with
// glamour-rendered markdown for assistant text.
type chatBuffer struct {
	spans []chatSpan

	// Streaming accumulators — the in-progress assistant / reasoning
	// span. When the corresponding finalize() fires, the accumulator
	// is flushed into spans and reset.
	pendingAssistant strings.Builder
	pendingReasoning strings.Builder

	renderer    *glamour.TermRenderer
	renderWidth int // width the cached renderer was constructed for
}

type chatSpanKind int

const (
	spanUser chatSpanKind = iota
	spanAssistant
	spanReasoning
	spanSystem
)

type chatSpan struct {
	kind  chatSpanKind
	label string
	text  string
}

func newChatBuffer() *chatBuffer {
	// Fixed style (no auto-detect). glamour.WithAutoStyle calls
	// termenv to query the terminal's background via OSC 11; the
	// response races bubbletea's stdin capture and gets echoed
	// into the textarea as garbage like `\11;rgb:1919/1a1a/1b1b\`.
	// Theme detection lands properly in slice 6 — for now ship a
	// dark default; light terminals stay readable but suboptimal.
	r, _ := glamour.NewTermRenderer(
		glamour.WithStandardStyle("dark"),
		glamour.WithWordWrap(80),
	)
	return &chatBuffer{renderer: r, renderWidth: 80}
}

func (b *chatBuffer) appendUser(name, text string) {
	b.spans = append(b.spans, chatSpan{kind: spanUser, label: name, text: text})
}

func (b *chatBuffer) appendAssistantChunk(text string) {
	b.pendingAssistant.WriteString(text)
}

func (b *chatBuffer) finalizeAssistant() {
	t := b.pendingAssistant.String()
	b.pendingAssistant.Reset()
	if t == "" {
		return
	}
	b.spans = append(b.spans, chatSpan{kind: spanAssistant, label: "hugen", text: t})
}

func (b *chatBuffer) appendReasoningChunk(text string) {
	b.pendingReasoning.WriteString(text)
}

func (b *chatBuffer) finalizeReasoning() {
	t := b.pendingReasoning.String()
	b.pendingReasoning.Reset()
	if t == "" {
		return
	}
	b.spans = append(b.spans, chatSpan{kind: spanReasoning, label: "thinking", text: t})
}

func (b *chatBuffer) appendSystem(text string) {
	b.spans = append(b.spans, chatSpan{kind: spanSystem, label: "system", text: text})
}

// render flattens the buffer into a string fit for the viewport.
// Width is the viewport's current width; the chatBuffer adjusts
// the glamour renderer's word-wrap to match.
func (b *chatBuffer) render(width int) string {
	if width <= 0 {
		width = 80
	}
	// Recreate the renderer ONLY when the viewport width changed.
	// glamour does not expose a setter and termenv queries the
	// terminal on construction; recreating per render flooded the
	// stdin with OSC 11 responses that bubbletea then echoed into
	// the textarea. WithStandardStyle skips the termenv query
	// entirely, so the only remaining cost is the goldmark parser
	// init — still worth caching.
	if b.renderer == nil || b.renderWidth != width {
		b.renderer, _ = glamour.NewTermRenderer(
			glamour.WithStandardStyle("dark"),
			glamour.WithWordWrap(width),
		)
		b.renderWidth = width
	}

	var sb strings.Builder
	for _, s := range b.spans {
		switch s.kind {
		case spanUser:
			sb.WriteString(styleUser.Render(s.label + " ❯ "))
			sb.WriteString(s.text)
			sb.WriteString("\n\n")
		case spanAssistant:
			if md, err := b.renderer.Render(s.text); err == nil {
				sb.WriteString(md)
			} else {
				sb.WriteString(s.text + "\n")
			}
		case spanReasoning:
			sb.WriteString(styleReasoning.Render("thinking: " + collapseLines(s.text)))
			sb.WriteString("\n\n")
		case spanSystem:
			sb.WriteString(styleSystem.Render("· " + s.text))
			sb.WriteString("\n\n")
		}
	}
	// Render the in-flight streaming chunks at the bottom — gives
	// the user immediate feedback while the assistant is still
	// emitting.
	if pa := b.pendingAssistant.String(); pa != "" {
		if md, err := b.renderer.Render(pa); err == nil {
			sb.WriteString(md)
		} else {
			sb.WriteString(pa + "\n")
		}
	}
	if pr := b.pendingReasoning.String(); pr != "" {
		sb.WriteString(styleReasoning.Render("thinking: " + collapseLines(pr)))
		sb.WriteString("\n")
	}
	return sb.String()
}

func collapseLines(s string) string {
	// Single-line render for reasoning streams in the chat pane;
	// keeps the transcript compact. Operators see the full
	// multi-line in the persisted event log.
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\n", " · ")
	return strings.TrimSpace(s)
}

var (
	styleUser      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	styleReasoning = lipgloss.NewStyle().Faint(true).Italic(true)
	styleSystem    = lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("8"))
)
