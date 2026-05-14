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

// tab is one root-session pane in the TUI. Slice 4 — multi-root
// support per phase-5.1c §8. Each tab owns its independent chat
// viewport, textarea contents, sidebar projection, HITL modal
// state, and dirty marker (activity since last focus).
//
// Globals (terminal size, user, logger) live on the parent [model];
// tab borrows them on demand via newTab + relayout.
type tab struct {
	sessionID string
	user      protocol.ParticipantInfo
	logger    *slog.Logger

	// submit is the adapter-supplied dispatcher. Every tab uses the
	// same closure; the runtime routes by the frame's SessionID.
	submit func(protocol.Frame) error

	viewport viewport.Model
	textarea textarea.Model

	chat        *chatBuffer
	closing     bool   // true once this tab issued /end
	statusLine  string // one-line footer status for this tab
	bannerError string // most recent error for this tab

	// Slice 2 projection (per-tab — different roots have different
	// activity).
	sidebarStatus *liveviewStatus

	// Slice 3 modal state (per-tab — only one inquiry per session).
	pendingInquiry *inquiryState

	// Slice 4 — dirty flips true when a frame lands on this tab
	// while it is NOT focused. Cleared when the tab takes focus.
	// Drives the tab-bar activity marker.
	dirty bool

	// Slice 6 — per-tab input history. Most-recent-first; capped
	// at maxHistoryPerTab. Loaded from ~/.hugen/tui.yaml on tab
	// attach; appended on every submit; persisted via the
	// adapter-installed historySaver callback.
	history    []string
	historyIdx int // -1 = not navigating

	// historySaver is invoked after history mutations so the
	// adapter can flush the in-memory slice to disk. Nil in
	// tests / when persistence is unavailable.
	historySaver func(sessionID string, history []string)

	// Slice 6 S2 — periodic SessionStats sample (event count).
	// -1 = "not sampled yet"; the footer renders nothing for
	// that case so the operator never sees a misleading "0
	// events" before the first sample lands.
	eventsCount int
}

// newTab builds a fresh tab bound to sessionID. The terminal
// geometry will be applied by the parent model's relayout call
// after construction.
func newTab(sessionID string, u protocol.ParticipantInfo, submit func(protocol.Frame) error, logger *slog.Logger) *tab {
	ta := textarea.New()
	ta.Placeholder = "Type your message; Enter to send, Shift+Enter for newline, Ctrl+C to exit"
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.SetHeight(inputHeight)
	ta.Prompt = "❯ "
	ta.Focus()
	ta.KeyMap.InsertNewline.SetEnabled(true)

	vp := viewport.New(80, 20) // resized on first WindowSizeMsg
	vp.SetContent("")
	// Clear viewport's default KeyMap so cursor keys don't double-fire
	// while textarea is focused. Scroll keys are routed explicitly.
	vp.KeyMap = viewport.KeyMap{}

	return &tab{
		sessionID:   sessionID,
		user:        u,
		logger:      logger,
		submit:      submit,
		viewport:    vp,
		textarea:    ta,
		chat:        newChatBuffer(),
		statusLine:  "ready",
		historyIdx:  -1,
		eventsCount: -1,
	}
}

// applyGeometry recomputes viewport + textarea sizing inside the
// parent model's reserved per-tab area. chatWidth is the width
// available for chat (sidebar already subtracted when shown);
// contentHeight is rows above the input + footer strip.
func (t *tab) applyGeometry(chatWidth, contentHeight, totalWidth int) {
	t.viewport.Width = chatWidth
	t.viewport.Height = contentHeight
	t.textarea.SetWidth(totalWidth)
	t.refreshChat()
}

// refreshChat re-renders the chat buffer into the viewport.
// Auto-follow to bottom only if the user was already pinned there —
// scrolling up to read history must NOT be undone by an incoming
// streaming chunk.
func (t *tab) refreshChat() {
	wasAtBottom := t.viewport.AtBottom()
	t.viewport.SetContent(t.chat.render(t.viewport.Width))
	if wasAtBottom {
		t.viewport.GotoBottom()
	}
}

// renderFooter is the bottom status line for this tab. Width is
// the total horizontal budget (chat + sidebar).
//
// Slice 6 S2 — when eventsCount has been sampled at least once,
// inject "· N events" before the status word so the operator
// sees session size at a glance. -1 (pre-sample) skips the
// segment to avoid a misleading "0 events" flash on attach.
func (t *tab) renderFooter(width int) string {
	prefix := fmt.Sprintf("session %s", shortID(t.sessionID))
	if t.eventsCount >= 0 {
		prefix += fmt.Sprintf(" · %s events", formatThousands(t.eventsCount))
	}
	prefix += " · " + t.statusLine
	left := lipgloss.NewStyle().Faint(true).Render(prefix)
	right := ""
	if t.bannerError != "" {
		right = lipgloss.NewStyle().Foreground(activeTheme.ErrorFG).Render(t.bannerError)
	}
	gap := strings.Repeat(" ", maxInt(1, width-lipgloss.Width(left)-lipgloss.Width(right)))
	return left + gap + right
}

// formatThousands renders n with comma separators ("1247" →
// "1,247"; "1247000" → "1,247,000"). Cheap; no localisation —
// the indicator is for power-user scan, not i18n.
func formatThousands(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	s := fmt.Sprintf("%d", n)
	// Walk right-to-left, insert "," every 3 digits.
	var b strings.Builder
	rem := len(s) % 3
	if rem > 0 {
		b.WriteString(s[:rem])
		if len(s) > rem {
			b.WriteByte(',')
		}
	}
	for i := rem; i < len(s); i += 3 {
		b.WriteString(s[i : i+3])
		if i+3 < len(s) {
			b.WriteByte(',')
		}
	}
	return b.String()
}

// appendUserBubble appends the user's outgoing message to the chat
// buffer and flushes any stale reasoning stripe from the previous
// turn (some model adapters never emit Reasoning{Final:true}).
func (t *tab) appendUserBubble(text string) {
	if t.chat.pendingReasoning.Len() > 0 {
		t.chat.finalizeReasoning()
	}
	t.chat.appendUser(t.user.Name, text)
	t.refreshChat()
}

// appendHistory prepends text to the tab's history ring (most-
// recent-first), caps at maxHistoryPerTab, and notifies the
// historySaver so the change persists to ~/.hugen/tui.yaml.
// De-duplicates back-to-back submissions of the same text.
func (t *tab) appendHistory(text string) {
	if text == "" {
		return
	}
	if len(t.history) > 0 && t.history[0] == text {
		t.historyIdx = -1
		return
	}
	out := make([]string, 0, len(t.history)+1)
	out = append(out, text)
	for _, prev := range t.history {
		if prev == text {
			continue // collapse older duplicates so cycling stays useful
		}
		out = append(out, prev)
		if len(out) >= maxHistoryPerTab {
			break
		}
	}
	t.history = out
	t.historyIdx = -1
	if t.historySaver != nil {
		t.historySaver(t.sessionID, t.history)
	}
}

// historyPrev loads the next-older entry into the textarea.
// historyIdx -1 (fresh state) jumps to entry 0; otherwise advance
// one step. No-op when already at the oldest entry.
func (t *tab) historyPrev() bool {
	if len(t.history) == 0 {
		return false
	}
	next := t.historyIdx + 1
	if next >= len(t.history) {
		return false
	}
	t.historyIdx = next
	t.textarea.SetValue(t.history[next])
	t.textarea.CursorEnd()
	return true
}

// historyNext walks back toward the present. At idx 0, one Down
// clears the textarea and resets idx to -1 — that returns the
// operator to "fresh input" mode.
func (t *tab) historyNext() bool {
	if t.historyIdx < 0 {
		return false
	}
	if t.historyIdx == 0 {
		t.historyIdx = -1
		t.textarea.Reset()
		return true
	}
	t.historyIdx--
	t.textarea.SetValue(t.history[t.historyIdx])
	t.textarea.CursorEnd()
	return true
}

// dispatchUserInput parses + submits one line of operator input.
// Slash commands become SlashCommand frames; plain text becomes
// UserMessage. The submit closure addresses the frame at this
// tab's sessionID.
func (t *tab) dispatchUserInput(text string) error {
	var f protocol.Frame
	if console.IsSlashCommand(text) {
		pc := console.ParseSlashCommand(text)
		f = protocol.NewSlashCommand(t.sessionID, t.user, pc.Name, pc.Args, pc.Raw)
	} else {
		f = protocol.NewUserMessage(t.sessionID, t.user, text)
	}
	return t.submit(f)
}

// dispatchInquiryKey handles keypresses while a HITL inquiry modal
// is open. Returns handled=true when the key produced a modal action;
// false when the key should fall through to default textarea
// handling. Slice 3 — phase 5.1c §7.
func (t *tab) dispatchInquiryKey(k tea.KeyMsg) (handled bool, cmd tea.Cmd) {
	pend := t.pendingInquiry
	if pend.replyMode {
		switch k.String() {
		case "enter":
			text := strings.TrimSpace(t.textarea.Value())
			if pend.req.Type == protocol.InquiryTypeClarification && text == "" {
				return true, nil
			}
			if err := t.submitInquiryReply(pend, text); err != nil {
				t.bannerError = err.Error()
			}
			return true, nil
		case "esc":
			if pend.req.Type == protocol.InquiryTypeApproval {
				pend.replyMode = false
				pend.replyVerb = ""
				t.textarea.Reset()
				return true, nil
			}
			t.pendingInquiry = nil
			t.textarea.Reset()
			t.bannerError = "inquiry dismissed (still pending server-side)"
			return true, nil
		}
		return false, nil
	}
	switch k.String() {
	case "y", "a":
		if err := t.submitInquiryReply(pend, "/approve"); err != nil {
			t.bannerError = err.Error()
		}
		return true, nil
	case "n", "d":
		if err := t.submitInquiryReply(pend, "/deny"); err != nil {
			t.bannerError = err.Error()
		}
		return true, nil
	case "r":
		pend.replyMode = true
		pend.replyVerb = "approve"
		t.textarea.Reset()
		t.textarea.Focus()
		return true, nil
	case "esc":
		t.pendingInquiry = nil
		t.bannerError = "inquiry dismissed (still pending server-side)"
		return true, nil
	}
	return false, nil
}

// submitInquiryReply builds the InquiryResponse via the shared
// console helper and submits it through the host. Clears the modal
// on success.
func (t *tab) submitInquiryReply(pend *inquiryState, line string) error {
	pi := &console.PendingInquiry{
		RequestID:       pend.req.RequestID,
		CallerSessionID: pend.req.CallerSessionID,
		Kind:            pend.req.Type,
	}
	resp, err := console.BuildInquiryReply(t.user, t.sessionID, pi, line)
	if err != nil {
		return err
	}
	if err := t.submit(resp); err != nil {
		return err
	}
	t.pendingInquiry = nil
	t.textarea.Reset()
	return nil
}

// updateKey routes a single keypress to the tab's chat / inquiry
// machinery. Returns (handled, cmd). `handled` means the key was
// consumed by the tab; otherwise the parent model can dispatch it
// elsewhere (e.g. tab-switch keys).
func (t *tab) updateKey(msg tea.KeyMsg) (handled bool, cmd tea.Cmd) {
	if t.closing {
		switch msg.String() {
		case "ctrl+c", "esc":
			// Caller (model) decides whether to quit or just close
			// this tab. Signal "handled" so it does not also fire
			// global tab logic.
			return true, tea.Quit
		}
		return true, nil
	}
	if t.pendingInquiry != nil {
		h, c := t.dispatchInquiryKey(msg)
		if h {
			return true, c
		}
		var taCmd tea.Cmd
		t.textarea, taCmd = t.textarea.Update(msg)
		return true, taCmd
	}
	switch msg.String() {
	case "pgup", "pgdown", "home", "end":
		var vpCmd tea.Cmd
		t.viewport, vpCmd = t.viewport.Update(msg)
		return true, vpCmd
	case "ctrl+c", "ctrl+d", "ctrl+w":
		t.statusLine = "closing…"
		t.closing = true
		cmd := protocol.NewSlashCommand(t.sessionID, t.user, "end", nil, "/end")
		if err := t.submit(cmd); err != nil {
			t.bannerError = fmt.Sprintf("submit /end: %v", err)
			return true, tea.Quit
		}
		return true, nil
	case "up":
		// Slice 6 — history navigation. Route to history when the
		// textarea is empty OR we're already in history-browsing
		// mode (so the operator can keep walking back). Otherwise
		// let textarea handle Up as a multi-line cursor move.
		if t.textarea.Value() == "" || t.historyIdx >= 0 {
			if t.historyPrev() {
				return true, nil
			}
			return true, nil
		}
	case "down":
		if t.historyIdx >= 0 {
			if t.historyNext() {
				return true, nil
			}
			return true, nil
		}
	case "enter":
		text := strings.TrimSpace(t.textarea.Value())
		if text == "" {
			return true, nil
		}
		t.textarea.Reset()
		t.appendUserBubble(text)
		t.appendHistory(text)
		if err := t.dispatchUserInput(text); err != nil {
			t.bannerError = err.Error()
		}
		return true, nil
	}
	// Default: forward to textarea + viewport for default editing.
	// If the operator was browsing history and now types something
	// that likely mutates the textarea content (a rune, backspace,
	// delete), reset historyIdx so the next Up starts from the
	// most recent entry instead of where the cursor stopped.
	if t.historyIdx >= 0 && isTextareaMutation(msg) {
		t.historyIdx = -1
	}
	var taCmd, vpCmd tea.Cmd
	t.textarea, taCmd = t.textarea.Update(msg)
	t.viewport, vpCmd = t.viewport.Update(msg)
	return true, tea.Batch(taCmd, vpCmd)
}

// isTextareaMutation reports whether a key likely mutates the
// textarea buffer — used to evict the operator from
// history-browse mode without false positives on cursor-only keys.
func isTextareaMutation(k tea.KeyMsg) bool {
	switch k.Type {
	case tea.KeyRunes, tea.KeySpace,
		tea.KeyBackspace, tea.KeyDelete,
		tea.KeyTab:
		return true
	}
	return false
}

// renderBody returns the tab's body: chat + optional sidebar above
// the input strip. Width is the chat width (sidebar excluded);
// totalWidth is for footer alignment; sidebarShown decides whether
// to compose the sidebar.
func (t *tab) renderBody(chatWidth, totalWidth int, sidebarShown bool) string {
	chat := t.viewport.View()
	top := chat
	if sidebarShown {
		contentW := sidebarWidth - 2
		if contentW < 1 {
			contentW = 1
		}
		side := sidebarBoxStyle.
			Width(contentW).
			Height(t.viewport.Height).
			Render(renderSidebar(t.sidebarStatus, contentW))
		top = lipgloss.JoinHorizontal(lipgloss.Top, chat, side)
	}
	if t.pendingInquiry != nil {
		modalW := chatWidth
		if modalW < 30 {
			modalW = 30
		}
		modal := renderInquiryModal(t.pendingInquiry, modalW)
		bottom := modal
		if t.pendingInquiry.replyMode {
			bottom = lipgloss.JoinVertical(lipgloss.Left, modal, t.textarea.View())
		}
		return lipgloss.JoinVertical(lipgloss.Left, top, bottom, t.renderFooter(totalWidth))
	}
	return lipgloss.JoinVertical(lipgloss.Left,
		top, t.textarea.View(), t.renderFooter(totalWidth))
}
