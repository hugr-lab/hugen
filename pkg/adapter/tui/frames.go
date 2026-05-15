package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/reflow/wordwrap"

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
		// Surface a few coordination tools as system markers so
		// the operator sees follow-up cascades / spawn lifecycles
		// in the chat transcript instead of just the sidebar.
		// Dogfood feedback: a typed follow-up triggers
		// session_notify_subagent silently — the operator has no
		// chat-visible confirmation that the cascade happened.
		switch v.Payload.Name {
		case "session_notify_subagent", "session:notify_subagent":
			t.chat.appendSystem(formatNotifySubagent(v.Payload.Args))
			t.refreshChat()
		case "session_spawn_mission", "session:spawn_mission":
			t.chat.appendSystem(formatSpawnMission(v.Payload.Args))
			t.refreshChat()
		}
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
	case *protocol.SubagentResult:
		// Phase 5.1c.async-root § TUI: announce when an async-spawned
		// mission lands on root's outbox. Sync results arrive via the
		// model's reply and need no transient marker; silent mode by
		// definition does not surface.
		if v.Payload.RenderMode == protocol.SubagentRenderAsyncNotify {
			t.chat.appendSystem(formatAsyncMissionCompleted(&v.Payload))
			t.refreshChat()
		}
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
			// Multi-line user messages (Shift+Enter newlines) +
			// long single lines used to overflow the viewport
			// horizontally. Wrap each line to (width-prefixWidth)
			// via reflow.wordwrap, then indent continuation lines
			// under the styled prefix using lipgloss.Width (raw
			// len() over-counts UTF-8 bytes for ❯).
			rawPrefix := s.label + " ❯ "
			prefixWidth := lipgloss.Width(rawPrefix)
			sb.WriteString(styleUser.Render(rawPrefix))
			sb.WriteString(indentBlock(
				wordwrap.String(s.text, maxInt(10, width-prefixWidth)),
				prefixWidth, false))
			sb.WriteString("\n\n")
		case spanAssistant:
			if md, err := b.renderer.Render(s.text); err == nil {
				sb.WriteString(md)
			} else {
				sb.WriteString(s.text + "\n")
			}
		case spanReasoning:
			const reasoningPrefix = "thinking: "
			wrapped := wordwrap.String(s.text, maxInt(10, width-len(reasoningPrefix)))
			sb.WriteString(styleReasoning.Render(prefixMultiline(reasoningPrefix, wrapped)))
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
		const reasoningPrefix = "thinking: "
		wrapped := wordwrap.String(pr, maxInt(10, width-len(reasoningPrefix)))
		sb.WriteString(styleReasoning.Render(prefixMultiline(reasoningPrefix, wrapped)))
		sb.WriteString("\n")
	}
	return sb.String()
}

// formatNotifySubagent makes a one-line summary for the chat
// system-marker when root forwards a follow-up to a running
// subagent. `args` is the tool's ToolCall Args (any-typed; the
// known shape is `{subagent_id, content}`). Quotes the content
// preview when present, falls back to a generic message.
func formatNotifySubagent(args any) string {
	m, _ := args.(map[string]any)
	target := "subagent"
	if id, ok := m["subagent_id"].(string); ok && id != "" {
		target = shortID(id)
	}
	preview := ""
	if c, ok := m["content"].(string); ok && c != "" {
		preview = c
		if len(preview) > 120 {
			preview = preview[:120] + "…"
		}
	}
	if preview == "" {
		return "📨 follow-up forwarded to " + target
	}
	return fmt.Sprintf("📨 follow-up → %s: %s", target, preview)
}

// formatAsyncMissionCompleted summarises a SubagentResult frame the
// runtime delivered through the async path (RenderMode ==
// "async_notify"). Phase 5.1c.async-root: the operator otherwise
// sees only the model's next reply referencing the mission's
// result; a transient marker makes the turnaround visible.
//
// Status badge picks ✓ / ✗ / ⊘ from the reason — completed runs
// land on ✓, anything else (cancelled, hard ceiling, error) on a
// muted glyph. Preview prefers Result; falls back to Goal so an
// empty-result completion still names what finished.
func formatAsyncMissionCompleted(p *protocol.SubagentResultPayload) string {
	badge := "✓"
	switch {
	case p.Reason == "" || p.Reason == protocol.TerminationCompleted:
		badge = "✓"
	case strings.HasPrefix(p.Reason, "subagent_cancel"),
		p.Reason == protocol.TerminationCancelCascade:
		badge = "⊘"
	default:
		badge = "✗"
	}
	preview := strings.TrimSpace(p.Result)
	if preview == "" {
		preview = strings.TrimSpace(p.Goal)
	}
	preview = strings.ReplaceAll(preview, "\n", " ")
	if len(preview) > 120 {
		preview = preview[:120] + "…"
	}
	tag := fmt.Sprintf("%s mission %s completed", badge, shortID(p.SessionID))
	if preview == "" {
		return tag
	}
	return tag + ": " + preview
}

// formatSpawnMission summarises a session_spawn_mission tool call
// for the chat transcript. Shows the skill + goal preview so the
// operator sees what the agent is delegating without scrolling
// the sidebar.
func formatSpawnMission(args any) string {
	m, _ := args.(map[string]any)
	skill := "mission"
	if s, ok := m["skill"].(string); ok && s != "" {
		skill = s
	}
	wait, _ := m["wait"].(string)
	preview := ""
	if g, ok := m["goal"].(string); ok && g != "" {
		preview = g
		if len(preview) > 120 {
			preview = preview[:120] + "…"
		}
	}
	tag := "🚀 spawning " + skill
	if wait != "" && wait != "sync" {
		tag += " (" + wait + ")"
	}
	if preview == "" {
		return tag
	}
	return tag + ": " + preview
}

// indentBlock indents each line of s by `pad` spaces. When
// includeFirst is true the first line is also indented; otherwise
// only continuation lines (used when the caller already wrote a
// styled prefix on the same line as the first content line).
func indentBlock(s string, pad int, includeFirst bool) string {
	if pad <= 0 || s == "" {
		return s
	}
	indent := strings.Repeat(" ", pad)
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	var sb strings.Builder
	for i, ln := range lines {
		if i > 0 {
			sb.WriteString("\n")
		}
		if i > 0 || includeFirst {
			sb.WriteString(indent)
		}
		sb.WriteString(ln)
	}
	return sb.String()
}

// prefixMultiline prepends `prefix` to the first line of s, then
// indents every subsequent line with whitespace of the same
// visual width so the block reads as one paragraph in the chat.
// Replaces the old collapseLines path — operators asked for the
// multi-line thinking trail rather than a flattened "· · ·" line.
func prefixMultiline(prefix, s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimRight(s, "\n")
	lines := strings.Split(s, "\n")
	indent := strings.Repeat(" ", len(prefix))
	var sb strings.Builder
	for i, ln := range lines {
		if i == 0 {
			sb.WriteString(prefix)
		} else {
			sb.WriteString("\n")
			sb.WriteString(indent)
		}
		sb.WriteString(ln)
	}
	return sb.String()
}

var (
	styleUser      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	styleReasoning = lipgloss.NewStyle().Faint(true).Italic(true)
	styleSystem    = lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("8"))
)
