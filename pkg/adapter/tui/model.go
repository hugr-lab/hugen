package tui

import (
	"log/slog"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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
	sidebarWidth       = 36
	sidebarMinTerminal = 80
)

// tabBarHeight reserves vertical space for the multi-root tab bar
// (slice 4). Always rendered, even with a single tab, so layout is
// stable across Ctrl+N / Ctrl+W.
const tabBarHeight = 1

// attachTabMsg adds a freshly opened tab to the model and switches
// focus to it. The pump goroutine for the new tab's subscription
// MUST start AFTER the model has appended the tab to m.tabs —
// otherwise the pump's first prog.Send for the new session id can
// arrive at handleFrame with no matching tab and the frame is
// silently dropped. To enforce this, the message carries the
// subscription channel and the reducer hands it to the adapter-
// installed startPump callback after the list mutation lands.
type attachTabMsg struct {
	t   *tab
	sub <-chan protocol.Frame
}

// openTabError surfaces an asynchronous OpenSession failure.
type openTabError struct{ err error }

// model is the multi-root Bubble Tea Model. Slice 4 — phase
// 5.1c §8. Per-tab state (chat, sidebar, inquiry, …) lives on
// [tab]; the model carries globals (terminal size, user, logger)
// and the tab list.
type model struct {
	user   protocol.ParticipantInfo
	logger *slog.Logger

	width, height int
	ready         bool
	sidebarShown  bool // depends on terminal width — global, not per-tab

	tabs   []*tab
	active int

	// openTab is the adapter-installed callback invoked on Ctrl+N.
	// Returns a tea.Cmd that opens a new root session, subscribes
	// to its outbox, spawns a pump, and yields an attachTabMsg with
	// the freshly minted tab. Nil in tests / when the adapter
	// doesn't support multi-root (initial single-tab fallback).
	openTab func() tea.Cmd

	// forgetTab is the adapter-installed callback invoked when a
	// tab is removed (operator close or session terminated). The
	// adapter updates ~/.hugen/tui.yaml so the dead root is not
	// re-attached on the next start. Nil in tests.
	forgetTab func(sessionID string)

	// sampleStats returns a tea.Cmd that, when run, fetches the
	// event count for the given session id and emits a
	// statsResultMsg. Slice 6 S2. Nil in tests.
	sampleStats func(sessionID string) tea.Cmd

	// startPump is invoked by the attachTabMsg reducer AFTER the
	// new tab has been appended to m.tabs. The adapter spawns a
	// goroutine that forwards frames from sub into the program;
	// because the tab is already in the list, the pump's first
	// Send is guaranteed to find a matching tab. Nil in tests.
	startPump func(sessionID string, sub <-chan protocol.Frame)
}

// statsResultMsg is the async reply from a sampleStats Cmd.
// Posted into the bubbletea loop; reducer locates the tab by
// sessionID and updates its footer counter.
type statsResultMsg struct {
	sessionID string
	events    int
	err       error
}

// tickStatsMsg fires periodically (statsTickInterval) — on
// receipt the model schedules a fresh SessionStats sample for
// every open tab and re-arms the timer.
type tickStatsMsg struct{}

// statsTickInterval is the cadence at which the footer's event
// count refreshes. 5s is the spec's recommendation — slow enough
// that the bucket aggregation cost is amortised, fast enough that
// the operator sees the counter move during sustained activity.
const statsTickInterval = 5 * time.Second

// newModel constructs a model with one initial tab. submit closure
// is shared across all tabs (the runtime routes by frame.SessionID).
func newModel(sessionID string, u protocol.ParticipantInfo, submit func(protocol.Frame) error, logger *slog.Logger) model {
	first := newTab(sessionID, u, submit, logger)
	return model{
		user:   u,
		logger: logger,
		tabs:   []*tab{first},
		active: 0,
	}
}

func (m model) Init() tea.Cmd {
	// Slice 6 S2 — schedule the first stats sample immediately so
	// the footer fills in within the first second of startup, and
	// arm the periodic tick.
	return tea.Batch(
		textarea.Blink,
		tea.Tick(time.Millisecond*200, func(time.Time) tea.Msg { return tickStatsMsg{} }),
	)
}

// currentTab returns the focused tab, or nil if the list is empty.
func (m *model) currentTab() *tab {
	if m.active < 0 || m.active >= len(m.tabs) {
		return nil
	}
	return m.tabs[m.active]
}

// findTab locates the tab with the given session id; returns
// (idx, *tab). idx == -1 when no match.
func (m *model) findTab(sessionID string) (int, *tab) {
	for i, t := range m.tabs {
		if t.sessionID == sessionID {
			return i, t
		}
	}
	return -1, nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch v := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = v.Width, v.Height
		m.relayout()
		m.ready = true
		return m, nil

	case attachTabMsg:
		m.tabs = append(m.tabs, v.t)
		m.active = len(m.tabs) - 1
		m.relayout()
		// Pump start MUST happen here, AFTER the append so any
		// frame the pump fans out can findTab() the new session.
		if m.startPump != nil && v.sub != nil {
			m.startPump(v.t.sessionID, v.sub)
		}
		// Kick off a stats sample for the new tab immediately so
		// its footer fills before the next tick.
		if m.sampleStats != nil {
			return m, m.sampleStats(v.t.sessionID)
		}
		return m, nil

	case tickStatsMsg:
		// Fan out one sample per tab and re-arm the timer.
		var cmds []tea.Cmd
		if m.sampleStats != nil {
			for _, t := range m.tabs {
				cmds = append(cmds, m.sampleStats(t.sessionID))
			}
		}
		cmds = append(cmds, tea.Tick(statsTickInterval, func(time.Time) tea.Msg { return tickStatsMsg{} }))
		return m, tea.Batch(cmds...)

	case statsResultMsg:
		if v.err == nil {
			if _, t := m.findTab(v.sessionID); t != nil {
				t.eventsCount = v.events
			}
		}
		return m, nil

	case openTabError:
		if cur := m.currentTab(); cur != nil {
			cur.bannerError = "open tab: " + v.err.Error()
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(v)

	case frameMsg:
		return m, m.handleFrame(v.frame)

	case errMsg:
		if cur := m.currentTab(); cur != nil {
			cur.bannerError = v.err.Error()
		}
		return m, nil
	}
	// Default: forward to active tab (so textarea / viewport tick
	// updates keep flowing).
	if cur := m.currentTab(); cur != nil {
		_, cmd := cur.updateKey(tea.KeyMsg{}) // no-op route through default
		return m, cmd
	}
	return m, nil
}

// handleKey processes a keypress. Tab-switch / open / close keys are
// handled here at the model level; everything else delegates to the
// focused tab.
func (m model) handleKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "ctrl+n":
		// Adapter callback opens the new session asynchronously.
		// The returned tea.Cmd is run by bubbletea and produces an
		// attachTabMsg (or openTabError) which the Update reducer
		// folds into the tab list.
		if m.openTab != nil {
			return m, m.openTab()
		}
		if cur := m.currentTab(); cur != nil {
			cur.bannerError = "open-tab callback not installed"
		}
		return m, nil
	case "ctrl+right", "ctrl+pgdown":
		// Forward cycle. ctrl+tab is not portable across terminals
		// (many emulators eat it); use these as the canonical bindings.
		if len(m.tabs) > 1 {
			m.active = (m.active + 1) % len(m.tabs)
			if cur := m.currentTab(); cur != nil {
				cur.dirty = false
			}
		}
		return m, nil
	case "ctrl+left", "ctrl+pgup":
		if len(m.tabs) > 1 {
			m.active = (m.active - 1 + len(m.tabs)) % len(m.tabs)
			if cur := m.currentTab(); cur != nil {
				cur.dirty = false
			}
		}
		return m, nil
	case "alt+1", "alt+2", "alt+3", "alt+4", "alt+5",
		"alt+6", "alt+7", "alt+8", "alt+9":
		// Direct 1-indexed switch. Alt is chosen over Ctrl
		// because Ctrl+N is reserved by VS Code's terminal for
		// its own tab switching; Alt+N stays portable across
		// Terminal.app / iTerm / Alacritty / Kitty / VS Code
		// integrated terminal. Out-of-range targets (e.g. Alt+9
		// with 3 tabs open) are no-ops so accidental presses
		// don't surprise the operator.
		idx := int(k.String()[len(k.String())-1] - '1') // '1' → 0, '9' → 8
		if idx >= 0 && idx < len(m.tabs) {
			m.active = idx
			if cur := m.currentTab(); cur != nil {
				cur.dirty = false
			}
		}
		return m, nil
	}
	cur := m.currentTab()
	if cur == nil {
		if k.String() == "ctrl+c" {
			return m, tea.Quit
		}
		return m, nil
	}
	_, cmd := cur.updateKey(k)
	return m, cmd
}

func (m model) View() string {
	if !m.ready {
		return "starting hugen TUI…"
	}
	cur := m.currentTab()
	if cur == nil {
		return "no active tab"
	}
	chatWidth := m.width
	if m.sidebarShown {
		chatWidth = m.width - sidebarWidth
	}
	bar := renderTabBar(m.tabs, m.active, m.width)
	body := cur.renderBody(chatWidth, m.width, m.sidebarShown)
	return lipgloss.JoinVertical(lipgloss.Left, bar, body)
}

// relayout recomputes geometry from the current terminal size and
// applies it to every tab. Reserved rows: tabBarHeight (top) +
// inputHeight + 1 footer row.
func (m *model) relayout() {
	reserved := tabBarHeight + inputHeight + 1
	contentHeight := m.height - reserved
	if contentHeight < 1 {
		contentHeight = 1
	}
	m.sidebarShown = m.width >= sidebarMinTerminal
	chatWidth := m.width
	if m.sidebarShown {
		chatWidth = m.width - sidebarWidth
		if chatWidth < 20 {
			m.sidebarShown = false
			chatWidth = m.width
		}
	}
	for _, t := range m.tabs {
		t.applyGeometry(chatWidth, contentHeight, m.width)
	}
}

// closeTab removes the tab at idx from the list and adjusts the
// active index. Used when SessionClosed arrives for a tab the
// runtime side terminated, or when the operator's /end ack returns.
// Returns tea.Quit when the last tab was closed.
func (m *model) closeTab(idx int) tea.Cmd {
	if idx < 0 || idx >= len(m.tabs) {
		return nil
	}
	closed := m.tabs[idx]
	m.tabs = append(m.tabs[:idx], m.tabs[idx+1:]...)
	if m.forgetTab != nil {
		m.forgetTab(closed.sessionID)
	}
	if len(m.tabs) == 0 {
		return tea.Quit
	}
	if m.active >= len(m.tabs) {
		m.active = len(m.tabs) - 1
	}
	return nil
}

var sidebarBoxStyle = lipgloss.NewStyle().
	BorderStyle(lipgloss.NormalBorder()).
	BorderLeft(true).
	BorderForeground(lipgloss.Color("8")).
	PaddingLeft(1)

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
