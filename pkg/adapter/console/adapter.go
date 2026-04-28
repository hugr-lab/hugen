package console

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/user"
	"strings"
	"sync"

	"github.com/hugr-lab/hugen/pkg/adapter"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/runtime"
)

// Adapter is the stdin/stdout REPL adapter. Single session per
// process; no multiplexing.
type Adapter struct {
	in     io.Reader
	out    io.Writer
	err    io.Writer
	resume string // optional: existing session id to resume

	logger *slog.Logger
	user   protocol.ParticipantInfo

	host    adapter.Host
	session *runtime.Session

	// Render state — track per-turn final newlines so we don't
	// double-print blank lines between Reasoning and AgentMessage.
	mu             sync.Mutex
	currentSection string // "" | "reasoning" | "agent"

	closed chan struct{} // closed when session_closed is observed
}

// Option configures the Adapter at construction.
type Option func(*Adapter)

// WithIO overrides the default stdin/stdout/stderr (used in tests).
func WithIO(in io.Reader, out, err io.Writer) Option {
	return func(a *Adapter) { a.in, a.out, a.err = in, out, err }
}

// WithUser overrides the default operator participant info (resolved
// from os/user).
func WithUser(p protocol.ParticipantInfo) Option {
	return func(a *Adapter) { a.user = p }
}

// WithLogger overrides the slog logger.
func WithLogger(l *slog.Logger) Option {
	return func(a *Adapter) { a.logger = l }
}

// WithResumeSession requests resume of an existing session id at
// startup (instead of opening a fresh one).
func WithResumeSession(id string) Option {
	return func(a *Adapter) { a.resume = id }
}

// New constructs a console Adapter with sensible defaults.
func New(opts ...Option) *Adapter {
	a := &Adapter{
		in:     os.Stdin,
		out:    os.Stdout,
		err:    os.Stderr,
		logger: slog.Default(),
		user:   defaultUserParticipant(),
		closed: make(chan struct{}),
	}
	for _, o := range opts {
		o(a)
	}
	return a
}

// Name implements adapter.Adapter.
func (a *Adapter) Name() string { return "console" }

// Run implements adapter.Adapter.
func (a *Adapter) Run(ctx context.Context, host adapter.Host) error {
	a.host = host
	if a.logger == nil {
		a.logger = host.Logger()
	}
	var session *runtime.Session
	var err error
	if a.resume != "" {
		session, err = host.ResumeSession(ctx, a.resume)
		if err != nil {
			return fmt.Errorf("console: resume %s: %w", a.resume, err)
		}
	} else {
		session, err = host.OpenSession(ctx, adapter.OpenRequest{
			OwnerID:      a.user.ID,
			Participants: []protocol.ParticipantInfo{a.user},
		})
		if err != nil {
			return fmt.Errorf("console: open: %w", err)
		}
	}
	a.session = session

	sub, err := host.Subscribe(ctx, session.ID())
	if err != nil {
		return fmt.Errorf("console: subscribe: %w", err)
	}

	fmt.Fprintf(a.out, "hugen console — session=%s. Type a message; Ctrl-D exits.\n", session.ID())
	fmt.Fprint(a.out, "> ")

	// Output goroutine: print Frames as they arrive.
	go a.runOutput(ctx, sub)

	// Input goroutine: read stdin, dispatch.
	return a.runInput(ctx)
}

func (a *Adapter) runInput(ctx context.Context) error {
	rd := bufio.NewReader(a.in)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line, err := rd.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				fmt.Fprintln(a.out)
				// Send /end so the session goroutine can persist a
				// session_closed marker and the binary exits cleanly.
				cmd := protocol.NewSlashCommand(a.session.ID(), a.user, "end", nil, "/end")
				if subErr := a.host.Submit(ctx, cmd); subErr != nil {
					a.logger.Warn("submit /end on EOF", "err", subErr)
					return nil
				}
				select {
				case <-a.closed:
				case <-ctx.Done():
				}
				return nil
			}
			return fmt.Errorf("console read: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.TrimSpace(line) == "" {
			fmt.Fprint(a.out, "> ")
			continue
		}
		var f protocol.Frame
		if IsSlashCommand(line) {
			pc := ParseSlashCommand(line)
			f = protocol.NewSlashCommand(a.session.ID(), a.user, pc.Name, pc.Args, pc.Raw)
		} else {
			f = protocol.NewUserMessage(a.session.ID(), a.user, line)
		}
		if err := a.host.Submit(ctx, f); err != nil {
			fmt.Fprintf(a.err, "submit: %v\n", err)
		}
		// /end short-circuits the input loop — wait for the session
		// to actually emit session_closed before returning so any
		// in-flight turn can finish persisting.
		if pc, ok := f.(*protocol.SlashCommand); ok && pc.Payload.Name == "end" {
			select {
			case <-a.closed:
			case <-ctx.Done():
			}
			return nil
		}
	}
}

func (a *Adapter) runOutput(ctx context.Context, sub <-chan protocol.Frame) {
	for {
		select {
		case <-ctx.Done():
			return
		case f, ok := <-sub:
			if !ok {
				return
			}
			a.render(f)
		}
	}
}

// render writes one Frame to stdout. State machine: track current
// section so streaming Reasoning + AgentMessage chunks print without
// duplicate prefixes; on section change or final, emit a newline.
func (a *Adapter) render(f protocol.Frame) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch v := f.(type) {
	case *protocol.UserMessage:
		// Echo of our own user message — skip; the prompt already
		// showed it.
		_ = v
	case *protocol.AgentMessage:
		if a.currentSection != "agent" {
			if a.currentSection != "" {
				fmt.Fprintln(a.out)
			}
			a.currentSection = "agent"
		}
		fmt.Fprint(a.out, v.Payload.Text)
		if v.Payload.Final {
			fmt.Fprintln(a.out)
			a.currentSection = ""
			fmt.Fprint(a.out, "> ")
		}
	case *protocol.Reasoning:
		if a.currentSection != "reasoning" {
			if a.currentSection != "" {
				fmt.Fprintln(a.out)
			}
			fmt.Fprint(a.out, "thinking: ")
			a.currentSection = "reasoning"
		}
		fmt.Fprint(a.out, v.Payload.Text)
		if v.Payload.Final {
			fmt.Fprintln(a.out)
			a.currentSection = ""
		}
	case *protocol.Error:
		if a.currentSection != "" {
			fmt.Fprintln(a.out)
			a.currentSection = ""
		}
		fmt.Fprintf(a.out, "error: %s\n", v.Payload.Message)
		fmt.Fprint(a.out, "> ")
	case *protocol.SystemMarker:
		if a.currentSection != "" {
			fmt.Fprintln(a.out)
			a.currentSection = ""
		}
		fmt.Fprintf(a.out, "system: %s\n", v.Payload.Subject)
		fmt.Fprint(a.out, "> ")
	case *protocol.SessionClosed:
		select {
		case <-a.closed:
		default:
			close(a.closed)
		}
	case *protocol.SessionOpened, *protocol.SessionSuspended,
		*protocol.SlashCommand, *protocol.Cancel, *protocol.Heartbeat:
		// Lifecycle / control frames are silent; the user can see
		// them in the persisted transcript.
	default:
		_ = v
	}
}

func defaultUserParticipant() protocol.ParticipantInfo {
	id := "operator"
	name := "operator"
	if u, err := user.Current(); err == nil && u != nil {
		if u.Username != "" {
			id = u.Username
			name = u.Username
		}
	}
	return protocol.ParticipantInfo{ID: id, Kind: protocol.ParticipantUser, Name: name}
}
