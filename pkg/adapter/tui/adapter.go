// Package tui implements the Bubble Tea TUI adapter — a rich
// reference client on top of the phase-5.1b liveview surface.
// Slice 1 ships chat-only parity with pkg/adapter/console for a
// single root session.
package tui

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/user"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hugr-lab/hugen/pkg/adapter"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

// Adapter is the Bubble Tea TUI adapter. Single root in slice 1;
// multi-root tabs added in slice 4.
type Adapter struct {
	resume string

	logger *slog.Logger
	user   protocol.ParticipantInfo

	host adapter.Host
	// initial holds the first root session opened by Run. Used to
	// wait on Done() at exit so the runtime's deferred Shutdown does
	// not race in-flight emits. Additional tabs (Ctrl+N) live only
	// inside the model + per-tab pump.
	initial *session.Session

	// persist is the in-memory mirror of ~/.hugen/tui.yaml. Slice 5
	// — mutated on tab open / close and re-flushed via saveSettings.
	// Nil only if the YAML file refused to load AND HomeDir is
	// unreachable; the absence is benign (persistence becomes a
	// no-op).
	persist *tuiSettings

	in  io.Reader
	out io.Writer
	err io.Writer
}

// Option configures the Adapter.
type Option func(*Adapter)

func WithLogger(l *slog.Logger) Option {
	return func(a *Adapter) { a.logger = l }
}

func WithUser(p protocol.ParticipantInfo) Option {
	return func(a *Adapter) { a.user = p }
}

func WithResumeSession(id string) Option {
	return func(a *Adapter) { a.resume = id }
}

// WithIO overrides stdin/stdout/stderr (tests).
func WithIO(in io.Reader, out, errOut io.Writer) Option {
	return func(a *Adapter) { a.in, a.out, a.err = in, out, errOut }
}

func New(opts ...Option) *Adapter {
	a := &Adapter{
		in:     os.Stdin,
		out:    os.Stdout,
		err:    os.Stderr,
		logger: slog.Default(),
		user:   defaultUserParticipant(),
	}
	for _, o := range opts {
		o(a)
	}
	return a
}

func (a *Adapter) Name() string { return "tui" }

// Run implements adapter.Adapter. Opens/resumes a root session,
// subscribes to its outbox, and starts the Bubble Tea program. The
// program pumps frames into the model via tea.Program.Send.
func (a *Adapter) Run(ctx context.Context, host adapter.Host) error {
	a.host = host
	if a.logger == nil {
		a.logger = host.Logger()
	}
	// Slice 5 — load persisted state (remembered root ids, theme,
	// …). A missing file is the first-run path; a corrupt file
	// degrades to empty so the TUI always starts. The resulting
	// settings struct is the source of truth for subsequent
	// rememberRoot / forgetRoot calls.
	if s, err := loadSettings(); err == nil {
		a.persist = s
	} else {
		a.logger.Warn("tui: load settings (continuing with defaults)", "err", err)
		a.persist = &tuiSettings{}
	}
	// Slice 6 — resolve + install theme BEFORE the model paints
	// its first frame. Precedence: user yaml override → COLORFGBG
	// auto-detect → dark default.
	resolved := resolveTheme(a.persist.Theme)
	applyTheme(resolved)
	a.logger.Debug("tui: theme applied", "name", resolved.Name)

	var sess *session.Session
	var err error
	if a.resume != "" {
		sess, err = host.ResumeSession(ctx, a.resume)
		if err != nil {
			return fmt.Errorf("tui: resume %s: %w", a.resume, err)
		}
	} else {
		sess, _, err = host.OpenSession(ctx, adapter.OpenRequest{
			OwnerID:      a.user.ID,
			Participants: []protocol.ParticipantInfo{a.user},
		})
		if err != nil {
			return fmt.Errorf("tui: open: %w", err)
		}
	}
	a.initial = sess

	sub, err := host.Subscribe(ctx, sess.ID())
	if err != nil {
		return fmt.Errorf("tui: subscribe: %w", err)
	}

	m := newModel(sess.ID(), a.user, a.submitFrame(ctx), a.logger)
	// Slice 5 — on-attach replay for the initial tab. Pull the
	// last replayLimit events from the persisted log and stitch
	// them into the chat buffer BEFORE bubbletea starts rendering
	// so the operator sees prior context immediately rather than
	// a blank pane that fills in only on the next live frame.
	// Errors degrade to "no history" — startup must not depend on
	// an event log being readable.
	if events, listErr := host.ListEvents(ctx, sess.ID(), store.ListEventsOpts{Limit: replayLimit}); listErr == nil {
		replayEvents(m.tabs[0], events)
	} else if a.logger != nil {
		a.logger.Warn("tui: initial replay skipped", "err", listErr)
	}
	// Slice 6 — seed input history + install the saver so every
	// submit persists.
	a.attachHistoryToTab(m.tabs[0])
	// Slice 5 — forget callback wired to persistence. Model invokes
	// it whenever a tab leaves the list (operator close or
	// SessionTerminated cascade).
	m.forgetTab = func(id string) { a.persistRoot(id, false) }
	// Slice 6 S2 — stats sampler. Returns a tea.Cmd so the call
	// runs on bubbletea's own goroutine (the host call itself
	// is synchronous; wrap it in a closure to avoid blocking the
	// reducer).
	m.sampleStats = func(id string) tea.Cmd {
		return func() tea.Msg {
			n, err := a.host.SessionStats(ctx, id)
			return statsResultMsg{sessionID: id, events: n, err: err}
		}
	}
	// Persist the initial tab id so a future restart re-attaches.
	a.persistRoot(sess.ID(), true)
	// Slice 4 — model emits requestOpenTabMsg on Ctrl+N. Wire the
	// open callback through the model so the resulting tea.Cmd
	// runs inside bubbletea's loop with access to host + the
	// program (for pump.Send). prog is constructed below; we close
	// over a pointer that we fill in after construction.
	var progRef *tea.Program
	m.openTab = func() tea.Cmd {
		return func() tea.Msg {
			return a.openNewTab(ctx, progRef)
		}
	}

	prog := tea.NewProgram(
		m,
		tea.WithContext(ctx),
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
		tea.WithInput(a.in),
		tea.WithOutput(a.out),
	)
	progRef = prog

	go a.pumpFrames(ctx, sub, prog)

	// Slice 5 — re-attach roots remembered from the previous run.
	// Each survivor becomes an additional tab seeded with its
	// recent event log. Dead roots are dropped from the persisted
	// list inside attachRememberedTabs. attachTabMsg MUST land in
	// the model BEFORE the per-tab pump starts forwarding live
	// frames; otherwise the first frame for the resumed session
	// could arrive at the dispatcher with no matching tab.
	go func() {
		if a.persist == nil {
			return
		}
		ids := append([]string(nil), a.persist.RecentRoots...)
		extras := a.attachRememberedTabs(ctx, ids)
		for _, e := range extras {
			prog.Send(attachTabMsg{t: e.t})
			go a.pumpFrames(ctx, e.sub, prog)
		}
	}()

	// Belt-and-suspenders terminal cleanup. bubbletea's own
	// shutdown disables mouse-tracking + exits altscreen on a clean
	// tea.Quit path, but the context-cancellation path
	// (tea.WithContext(ctx)) can race past that cleanup, leaving
	// the shell stuck in SGR mouse-tracking mode — every mouse
	// scroll after exit then types raw event bytes like
	// `65;142;9M65;142;9M…` into the user's prompt. defer ordered
	// LIFO so the raw CSI emit runs last — idempotent, safe to
	// emit even after bubbletea already disabled everything.
	defer fmt.Fprint(a.out,
		"\x1b[?1006l"+ // disable SGR mouse mode
			"\x1b[?1002l"+ // disable cell-motion tracking
			"\x1b[?1003l"+ // disable any-motion tracking
			"\x1b[?1049l"+ // exit altscreen buffer
			"\x1b[?25h", // show cursor
	)
	defer func() { _ = prog.ReleaseTerminal() }()

	if _, err := prog.Run(); err != nil {
		return fmt.Errorf("tui: program: %w", err)
	}

	// Wait for the session goroutine to fully exit so the caller's
	// deferred runtime.Shutdown does not race rootCancel against an
	// in-flight emit. Mirrors the console adapter's EOF / /end path.
	select {
	case <-a.initial.Done():
	case <-ctx.Done():
	}
	return nil
}

// pumpFrames forwards subscription frames into the Bubble Tea
// program. tea.Program.Send is goroutine-safe. Exits when the
// subscription channel closes or ctx cancels.
func (a *Adapter) pumpFrames(ctx context.Context, sub <-chan protocol.Frame, prog *tea.Program) {
	for {
		select {
		case <-ctx.Done():
			return
		case f, ok := <-sub:
			if !ok {
				return
			}
			prog.Send(frameMsg{frame: f})
		}
	}
}

// submitFrame returns a closure the model can call to submit a
// frame to the runtime. Errors are surfaced as errMsg in the model.
func (a *Adapter) submitFrame(ctx context.Context) func(protocol.Frame) error {
	return func(f protocol.Frame) error {
		return a.host.Submit(ctx, f)
	}
}

// openNewTab opens a fresh root session, subscribes to its outbox,
// spawns a pump goroutine, and returns an attachTabMsg the model
// folds into its tab list. Errors are surfaced as openTabError so
// the active tab can flash the failure in its banner. Slice 4 +
// slice 5: persists the new root in ~/.hugen/tui.yaml.
func (a *Adapter) openNewTab(ctx context.Context, prog *tea.Program) tea.Msg {
	sess, _, err := a.host.OpenSession(ctx, adapter.OpenRequest{
		OwnerID:      a.user.ID,
		Participants: []protocol.ParticipantInfo{a.user},
	})
	if err != nil {
		return openTabError{err: fmt.Errorf("OpenSession: %w", err)}
	}
	sub, err := a.host.Subscribe(ctx, sess.ID())
	if err != nil {
		return openTabError{err: fmt.Errorf("Subscribe: %w", err)}
	}
	go a.pumpFrames(ctx, sub, prog)
	t := newTab(sess.ID(), a.user, a.submitFrame(ctx), a.logger)
	a.attachHistoryToTab(t)
	a.persistRoot(sess.ID(), true)
	return attachTabMsg{t: t}
}

// rememberedAttach is one entry returned by attachRememberedTabs:
// the freshly minted tab plus the subscription channel its pump
// will consume from. The caller is responsible for sending the
// attachTabMsg FIRST and starting the pump after — that ordering
// guarantees the model has the tab in its list before any live
// frame arrives for the session id.
type rememberedAttach struct {
	t   *tab
	sub <-chan protocol.Frame
}

// attachRememberedTabs walks the persisted root id list from
// ~/.hugen/tui.yaml and tries to ResumeSession each one. Surviving
// roots become tabs (with their event log replayed into the chat
// buffer); dead roots are dropped from the settings file with a
// single info-log line so the operator sees what was forgotten.
func (a *Adapter) attachRememberedTabs(ctx context.Context, ids []string) []rememberedAttach {
	if len(ids) == 0 {
		return nil
	}
	var attached []rememberedAttach
	survivors := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || id == a.initial.ID() {
			continue // skip duplicates of the initial tab
		}
		sess, err := a.host.ResumeSession(ctx, id)
		if err != nil {
			if a.logger != nil {
				a.logger.Info("tui: remembered root not resumable; dropping", "id", id, "err", err)
			}
			continue
		}
		sub, subErr := a.host.Subscribe(ctx, sess.ID())
		if subErr != nil {
			if a.logger != nil {
				a.logger.Warn("tui: subscribe failed for remembered root", "id", id, "err", subErr)
			}
			continue
		}
		t := newTab(sess.ID(), a.user, a.submitFrame(ctx), a.logger)
		if events, listErr := a.host.ListEvents(ctx, sess.ID(), store.ListEventsOpts{Limit: replayLimit}); listErr == nil {
			replayEvents(t, events)
		}
		a.attachHistoryToTab(t)
		attached = append(attached, rememberedAttach{t: t, sub: sub})
		survivors = append(survivors, id)
	}
	// Rewrite the settings file so dead roots are forgotten on the
	// next start. Best-effort.
	if a.persist != nil {
		a.persist.RecentRoots = append([]string{a.initial.ID()}, survivors...)
		_ = saveSettings(a.persist)
	}
	return attached
}

// persistRoot upserts (add=true) or removes (add=false) a session
// id from the persisted recent-roots list. Best-effort — errors
// are logged-and-swallowed; persistence is a UX nicety, never a
// correctness lever.
func (a *Adapter) persistRoot(id string, add bool) {
	if a.persist == nil {
		return
	}
	if add {
		a.persist.RecentRoots = rememberRoot(a.persist.RecentRoots, id)
	} else {
		a.persist.RecentRoots = forgetRoot(a.persist.RecentRoots, id)
		if a.persist.History != nil {
			delete(a.persist.History, id)
		}
	}
	if err := saveSettings(a.persist); err != nil && a.logger != nil {
		a.logger.Warn("tui: save settings", "err", err)
	}
}

// attachHistoryToTab loads the persisted input history for the
// tab's session ID and installs the historySaver callback so the
// in-memory ring flushes back to disk on every submit. Slice 6.
func (a *Adapter) attachHistoryToTab(t *tab) {
	if a.persist == nil || t == nil {
		return
	}
	if a.persist.History != nil {
		if h, ok := a.persist.History[t.sessionID]; ok {
			t.history = append([]string(nil), h...)
		}
	}
	t.historySaver = func(sid string, hist []string) {
		if a.persist == nil {
			return
		}
		if a.persist.History == nil {
			a.persist.History = map[string][]string{}
		}
		a.persist.History[sid] = append([]string(nil), hist...)
		if err := saveSettings(a.persist); err != nil && a.logger != nil {
			a.logger.Warn("tui: save history", "err", err)
		}
	}
}

func defaultUserParticipant() protocol.ParticipantInfo {
	id := "operator"
	name := "operator"
	if u, err := user.Current(); err == nil && u != nil && u.Username != "" {
		id = u.Username
		name = u.Username
	}
	return protocol.ParticipantInfo{ID: id, Kind: protocol.ParticipantUser, Name: name}
}
