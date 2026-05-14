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
)

// Adapter is the Bubble Tea TUI adapter. Single-root in slice 1;
// multi-root tabs land in slice 4.
type Adapter struct {
	resume string

	logger *slog.Logger
	user   protocol.ParticipantInfo

	host    adapter.Host
	session *session.Session

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
	a.session = sess

	sub, err := host.Subscribe(ctx, sess.ID())
	if err != nil {
		return fmt.Errorf("tui: subscribe: %w", err)
	}

	m := newModel(sess.ID(), a.user, a.submitFrame(ctx), a.logger)

	prog := tea.NewProgram(
		m,
		tea.WithContext(ctx),
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
		tea.WithInput(a.in),
		tea.WithOutput(a.out),
	)

	go a.pumpFrames(ctx, sub, prog)

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
	case <-a.session.Done():
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

func defaultUserParticipant() protocol.ParticipantInfo {
	id := "operator"
	name := "operator"
	if u, err := user.Current(); err == nil && u != nil && u.Username != "" {
		id = u.Username
		name = u.Username
	}
	return protocol.ParticipantInfo{ID: id, Kind: protocol.ParticipantUser, Name: name}
}
