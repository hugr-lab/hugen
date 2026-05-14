package tui

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/hugr-lab/hugen/pkg/adapter/console"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// inputHeight is the bottom textarea's fixed row count. 3 rows fit
// most prompts; Shift+Enter inserts a newline, scrolling within.
const inputHeight = 3

// sidebar geometry constants.
//
//   - sidebarWidth: total cells the sidebar (including its 1-cell
//     left border) occupies when shown.
//   - sidebarMinTerminal: below this terminal width the sidebar
//     collapses entirely (chat-only fallback for very narrow
//     terminals; spec open Q #6).
const (
	sidebarWidth      = 36
	sidebarMinTerminal = 80
)

// model is the Bubble Tea Model for the TUI adapter. Slice 1 is
// single-tab; multi-root tabs land in slice 4.
type model struct {
	sessionID string
	user      protocol.ParticipantInfo
	logger    *slog.Logger

	submit func(protocol.Frame) error // adapter-supplied submit closure

	width, height int
	ready         bool

	viewport viewport.Model
	textarea textarea.Model

	chat        *chatBuffer
	closing     bool   // true once the user has issued /end
	statusLine  string // one-line footer status (e.g. "thinking…")
	bannerError string // most recent submit / runtime error

	// Slice 2 — sidebar projection. Replaced wholesale on every
	// incoming liveview/status frame; render is a pure function
	// of the most recent value (no accumulated state).
	sidebarStatus *liveviewStatus
	sidebarShown  bool // false when terminal < sidebarMinTerminal
}

func newModel(sessionID string, u protocol.ParticipantInfo, submit func(protocol.Frame) error, logger *slog.Logger) model {
	ta := textarea.New()
	ta.Placeholder = "Type your message; Enter to send, Shift+Enter for newline, Ctrl+C to exit"
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.SetHeight(inputHeight)
	ta.Prompt = "❯ "
	ta.Focus()

	// Disable bubbletea's default Ctrl+C/Ctrl+D — we want to intercept
	// them and trigger /end ourselves (the runtime needs a clean close).
	ta.KeyMap.InsertNewline.SetEnabled(true)

	vp := viewport.New(80, 20) // resized on first WindowSizeMsg
	vp.SetContent("")
	// Clear viewport's default KeyMap so Up/Down/PgUp/etc. stop
	// double-firing while textarea is focused. Slice 1 routes
	// PgUp / PgDown / Home / End explicitly in Update — mouse wheel
	// still works via tea.WithMouseCellMotion.
	vp.KeyMap = viewport.KeyMap{}

	return model{
		sessionID:  sessionID,
		user:       u,
		logger:     logger,
		submit:     submit,
		viewport:   vp,
		textarea:   ta,
		chat:       newChatBuffer(),
		statusLine: "ready",
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(textarea.Blink)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var (
		taCmd tea.Cmd
		vpCmd tea.Cmd
	)

	switch v := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = v.Width, v.Height
		m.relayout()
		m.ready = true
		return m, nil

	case tea.KeyMsg:
		// Closing state: only Ctrl+C / esc force-quit; everything else
		// is ignored while we wait for session_terminated.
		if m.closing {
			switch v.String() {
			case "ctrl+c", "esc":
				return m, tea.Quit
			}
			return m, nil
		}
		switch v.String() {
		case "pgup", "pgdown", "home", "end":
			// Route scroll keys to the viewport ONLY — textarea
			// gets cursor keys (up/down/left/right) for editing.
			m.viewport, vpCmd = m.viewport.Update(v)
			return m, vpCmd
		case "ctrl+c", "ctrl+d":
			// Submit /end, transition to closing, wait for terminator.
			m.statusLine = "closing…"
			m.closing = true
			cmd := protocol.NewSlashCommand(m.sessionID, m.user, "end", nil, "/end")
			if err := m.submit(cmd); err != nil {
				m.bannerError = fmt.Sprintf("submit /end: %v", err)
				return m, tea.Quit
			}
			return m, nil
		case "enter":
			// Send the current textarea value as a UserMessage (or
			// SlashCommand if prefixed with /). Shift+Enter is handled
			// by textarea's KeyMap.InsertNewline and never reaches
			// here as the literal "enter" string.
			text := strings.TrimSpace(m.textarea.Value())
			if text == "" {
				return m, nil
			}
			m.textarea.Reset()
			m.appendUserBubble(text)
			if err := m.dispatchUserInput(text); err != nil {
				m.bannerError = err.Error()
			}
			return m, nil
		}

	case frameMsg:
		return m, m.handleFrame(v.frame)

	case errMsg:
		m.bannerError = v.err.Error()
		return m, nil
	}

	m.textarea, taCmd = m.textarea.Update(msg)
	m.viewport, vpCmd = m.viewport.Update(msg)
	return m, tea.Batch(taCmd, vpCmd)
}

func (m model) View() string {
	if !m.ready {
		return "starting hugen TUI…"
	}
	chat := m.viewport.View()
	top := chat
	if m.sidebarShown {
		// Lipgloss adds border + padding cells ON TOP of Width().
		// Total rendered width = Width + PaddingLeft + BorderLeft.
		// We want the rendered block to consume exactly sidebarWidth
		// cells so the chat (width - sidebarWidth) abuts cleanly.
		contentW := sidebarWidth - 2 // 1 cell border + 1 cell padding
		if contentW < 1 {
			contentW = 1
		}
		side := sidebarBoxStyle.
			Width(contentW).
			Height(m.viewport.Height).
			Render(renderSidebar(m.sidebarStatus, contentW))
		top = lipgloss.JoinHorizontal(lipgloss.Top, chat, side)
	}
	input := m.textarea.View()
	footer := m.renderFooter()
	return lipgloss.JoinVertical(lipgloss.Left, top, input, footer)
}

// relayout adjusts viewport + textarea + sidebar geometry to the
// current terminal size. Reserved rows: 1 for footer, inputHeight
// for textarea. Sidebar shows when terminal width ≥
// sidebarMinTerminal; below that, chat occupies the full width.
func (m *model) relayout() {
	reserved := inputHeight + 1
	contentHeight := m.height - reserved
	if contentHeight < 1 {
		contentHeight = 1
	}

	m.sidebarShown = m.width >= sidebarMinTerminal
	chatWidth := m.width
	if m.sidebarShown {
		chatWidth = m.width - sidebarWidth
		if chatWidth < 20 {
			// Fall back to chat-only if the sidebar would crowd the
			// chat below readable width.
			m.sidebarShown = false
			chatWidth = m.width
		}
	}

	m.viewport.Width = chatWidth
	m.viewport.Height = contentHeight
	m.textarea.SetWidth(m.width)
	m.refreshChat()
}

var sidebarBoxStyle = lipgloss.NewStyle().
	BorderStyle(lipgloss.NormalBorder()).
	BorderLeft(true).
	BorderForeground(lipgloss.Color("8")).
	PaddingLeft(1)

// refreshChat re-renders the entire chat buffer into the viewport.
// Called on layout change or after appending a new line. Auto-follow
// to bottom only if the user was already pinned to the bottom —
// scrolling up to read history must NOT be undone by an incoming
// streaming chunk.
func (m *model) refreshChat() {
	wasAtBottom := m.viewport.AtBottom()
	m.viewport.SetContent(m.chat.render(m.viewport.Width))
	if wasAtBottom {
		m.viewport.GotoBottom()
	}
}

// renderFooter is the single-line bottom status. Includes the
// session id (truncated) and the current status. Errors flash in
// red until cleared by the next frame.
func (m *model) renderFooter() string {
	left := lipgloss.NewStyle().Faint(true).Render(fmt.Sprintf("session %s · %s", shortID(m.sessionID), m.statusLine))
	right := ""
	if m.bannerError != "" {
		right = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Render(m.bannerError)
	}
	gap := strings.Repeat(" ", maxInt(1, m.width-lipgloss.Width(left)-lipgloss.Width(right)))
	return left + gap + right
}

func (m *model) appendUserBubble(text string) {
	// Sending a new user message implicitly ends the previous turn —
	// flush any stuck pending reasoning so the chat does not retain
	// last turn's "thinking…" stripe between submission and the
	// runtime's first response frame.
	if m.chat.pendingReasoning.Len() > 0 {
		m.chat.finalizeReasoning()
	}
	m.chat.appendUser(m.user.Name, text)
	m.refreshChat()
}

func (m *model) dispatchUserInput(text string) error {
	var f protocol.Frame
	if console.IsSlashCommand(text) {
		pc := console.ParseSlashCommand(text)
		f = protocol.NewSlashCommand(m.sessionID, m.user, pc.Name, pc.Args, pc.Raw)
	} else {
		f = protocol.NewUserMessage(m.sessionID, m.user, text)
	}
	return m.submit(f)
}

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
