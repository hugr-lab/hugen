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

// handleFrame routes one incoming frame to the matching tab by
// session id. Frames addressed at non-active tabs flip the tab's
// `dirty` marker for the tab-bar activity indicator. Returns a
// tea.Cmd when the frame triggers a Bubble Tea action (e.g.
// SessionClosed → tab removal / quit during shutdown).
func (m *model) handleFrame(f protocol.Frame) tea.Cmd {
	idx, t := m.findTab(f.SessionID())
	if t == nil {
		// Single-tab fallback for tests that mint frames without
		// setting BaseFrame.Session. In production this branch
		// is a no-op because (a) subscriptions emit frames with
		// the correct session id, and (b) M1 ensures every tab
		// is in m.tabs before its pump starts Sending. Multi-tab
		// + unmatched id ⇒ drop+log (stale frame for a tab that
		// was just closed; the runtime fan-out hasn't seen our
		// unsubscribe yet).
		if len(m.tabs) == 1 {
			idx, t = 0, m.tabs[0]
		} else {
			if m.logger != nil {
				m.logger.Debug("tui: dropped frame for unknown session",
					"session", f.SessionID(), "kind", string(f.Kind()))
			}
			return nil
		}
	}
	cmd := t.handleFrame(f)
	if idx != m.active {
		t.dirty = true
	}
	// Tab-removal hooks fire when SessionTerminated lands.
	if _, ok := f.(*protocol.SessionTerminated); ok {
		// Defer removal one tick so the operator briefly sees the
		// "session terminated (reason)" marker. For now keep it
		// open — slice 4 minimal: only operator-initiated /end via
		// closing flag triggers actual removal. SessionClosed below
		// is the close ack.
	}
	if _, ok := f.(*protocol.SessionClosed); ok && t.closing {
		// Operator closed this tab. Remove it; tea.Quit if it was
		// the last.
		if rem := m.closeTab(idx); rem != nil {
			return rem
		}
	}
	return cmd
}

// handleFrame is the per-tab projection of one incoming frame.
// Returns tea.Quit when the tab's operator-initiated close has
// settled (the model layer decides whether to remove the tab or
// also exit the program).
func (t *tab) handleFrame(f protocol.Frame) tea.Cmd {
	// Auto-flush in-flight reasoning when a non-reasoning frame
	// arrives. Some model adapters never emit Reasoning{Final:true}
	// at end-of-stream; without this guard the pendingReasoning
	// accumulator persists across turns and the chat shows last
	// turn's "thinking…" at the bottom forever.
	if _, isReasoning := f.(*protocol.Reasoning); !isReasoning {
		if t.chat.pendingReasoning.Len() > 0 {
			t.chat.finalizeReasoning()
		}
	}
	switch v := f.(type) {
	case *protocol.UserMessage:
		// Already echoed via appendUserBubble on submit.
		return nil
	case *protocol.AgentMessage:
		t.handleAgentMessage(v)
	case *protocol.Reasoning:
		t.handleReasoning(v)
	case *protocol.ToolCall:
		t.statusLine = fmt.Sprintf("tool: %s", v.Payload.Name)
	case *protocol.ToolResult:
		t.statusLine = "thinking…"
	case *protocol.Error:
		t.bannerError = fmt.Sprintf("%s: %s", v.Payload.Code, v.Payload.Message)
		t.statusLine = "ready"
	case *protocol.SystemMarker:
		t.chat.appendSystem(v.Payload.Subject)
		t.refreshChat()
	case *protocol.ExtensionFrame:
		if v.Payload.Extension == "liveview" && v.Payload.Op == "status" {
			if st, err := parseLiveviewStatus(v.Payload.Data); err == nil {
				t.sidebarStatus = st
			}
		}
	case *protocol.InquiryRequest:
		t.pendingInquiry = newInquiryState(v)
		t.statusLine = fmt.Sprintf("HITL: %s", v.Payload.Type)
		t.textarea.Reset()
		t.textarea.Focus()
	case *protocol.InquiryResponse:
		t.pendingInquiry = nil
		t.statusLine = "ready"
	case *protocol.SessionTerminated:
		t.chat.appendSystem(fmt.Sprintf("session terminated (%s)", v.Payload.Reason))
		t.refreshChat()
		t.pendingInquiry = nil
	case *protocol.SessionClosed:
		// Model-layer logic decides what to do (close the tab if
		// the operator initiated, else leave the chat readable).
		t.statusLine = "closed"
	}
	return nil
}

func (t *tab) handleAgentMessage(v *protocol.AgentMessage) {
	if v.Payload.Consolidated {
		if v.Payload.Final {
			t.chat.finalizeAssistant()
			t.refreshChat()
			t.statusLine = "ready"
		}
		return
	}
	t.chat.appendAssistantChunk(v.Payload.Text)
	t.statusLine = "thinking…"
	t.refreshChat()
}

func (t *tab) handleReasoning(v *protocol.Reasoning) {
	if v.Payload.Text == "" {
		return
	}
	t.chat.appendReasoningChunk(v.Payload.Text)
	if v.Payload.Final {
		t.chat.finalizeReasoning()
	}
	t.refreshChat()
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
