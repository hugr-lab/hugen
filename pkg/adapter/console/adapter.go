package console

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/user"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/hugr-lab/hugen/pkg/adapter"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session"
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
	session *session.Session

	// Render state — track per-turn final newlines so we don't
	// double-print blank lines between Reasoning and AgentMessage.
	mu             sync.Mutex
	currentSection string // "" | "reasoning" | "agent"

	// pending holds the in-flight HITL inquiry the user is being
	// asked to answer. Written from the render goroutine when an
	// InquiryRequest lands; read from the input goroutine before
	// dispatching each line. nil when no inquiry is open.
	// Phase 5.1 § 2.
	pending atomic.Pointer[PendingInquiry]

	// compactorMarkerDisabled tracks the operator's
	// `compactor.ui_marker.enabled` toggle, mirrored from the
	// most-recent liveview/status frame's `extensions.compactor`
	// projection. Default is "enabled" (zero value = false → not
	// disabled); a status frame with ui_marker_enabled=false
	// flips this to true and suppresses subsequent markers.
	// Phase 5.2 δ.
	compactorMarkerDisabled atomic.Bool

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
	var session *session.Session
	var err error
	if a.resume != "" {
		session, err = host.ResumeSession(ctx, a.resume)
		if err != nil {
			return fmt.Errorf("console: resume %s: %w", a.resume, err)
		}
	} else {
		session, _, err = host.OpenSession(ctx, adapter.OpenRequest{
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
				// Wait for the session goroutine to fully exit
				// (s.done closes after teardown writes
				// session_terminated). Returning on a.closed —
				// the SessionClosed FRAME — is too early: the
				// goroutine is still mid-requestClose, and our
				// caller's deferred runtime.Shutdown would race
				// rootCancel against the in-flight emit.
				select {
				case <-a.session.Done():
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
		if pend := a.pending.Load(); pend != nil {
			if a.maybeHandleInquiryReply(ctx, pend, line) {
				continue
			}
		}
		var f protocol.Frame
		if IsSlashCommand(line) {
			pc := ParseSlashCommand(line)
			// Inquiry-reply commands typed outside an inquiry
			// context would otherwise fall through to the runtime
			// as opaque SlashCommand frames the handler doesn't
			// recognise, leaving the user with no feedback. Echo
			// once and re-prompt instead.
			switch pc.Name {
			case "approve", "deny", "respond":
				fmt.Fprintf(a.err, "no pending inquiry — /%s is only valid when prompted\n", pc.Name)
				fmt.Fprint(a.out, "> ")
				continue
			}
			f = protocol.NewSlashCommand(a.session.ID(), a.user, pc.Name, pc.Args, pc.Raw)
		} else {
			f = protocol.NewUserMessage(a.session.ID(), a.user, line)
		}
		if err := a.host.Submit(ctx, f); err != nil {
			fmt.Fprintf(a.err, "submit: %v\n", err)
		}
		// /end short-circuits the input loop — wait for the session
		// goroutine to fully exit (Done closes after teardown
		// writes session_terminated). Waiting on the SessionClosed
		// FRAME (a.closed) is too early: the goroutine is still
		// mid-requestClose and the caller's deferred
		// runtime.Shutdown would race rootCancel against the
		// in-flight emit.
		if pc, ok := f.(*protocol.SlashCommand); ok && pc.Payload.Name == "end" {
			select {
			case <-a.session.Done():
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
		// Consolidated rows carry the same text already streamed via
		// chunks (Consolidated=false). Re-printing would duplicate
		// the assistant's output on screen. Treat them as markers:
		// Final=true draws the newline + prompt cut; Final=false
		// (tool-iteration) is silent — the dispatcher's tool_call
		// rendering follows.
		if v.Payload.Consolidated {
			if v.Payload.Final {
				if a.currentSection != "" {
					fmt.Fprintln(a.out)
				}
				a.currentSection = ""
				fmt.Fprint(a.out, "> ")
			}
			break
		}
		if a.currentSection != "agent" {
			if a.currentSection != "" {
				fmt.Fprintln(a.out)
			}
			a.currentSection = "agent"
		}
		fmt.Fprint(a.out, v.Payload.Text)
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
	case *protocol.InquiryRequest:
		a.renderInquiryRequest(v)
	case *protocol.InquiryResponse:
		// Routing/echo frame — silent. The agent's resumed turn
		// renders the model output.
	case *protocol.SessionClosed:
		select {
		case <-a.closed:
		default:
			close(a.closed)
		}
	case *protocol.ExtensionFrame:
		a.renderExtensionFrame(v)
	case *protocol.SessionOpened,
		*protocol.SlashCommand, *protocol.Cancel, *protocol.Heartbeat:
		// Lifecycle / control frames are silent; the user can see
		// them in the persisted transcript.
	default:
		_ = v
	}
}

// renderExtensionFrame draws inline markers for extension-driven
// signals the operator should see in the transcript. Phase 5.2 δ —
// compactor's `digest_set` op produces a faint horizontal-rule
// marker at the cutoff boundary; liveview's `status` op updates the
// `compactor.ui_marker_enabled` cache so the marker honours the
// operator's `compactor.ui_marker.enabled` toggle in real time.
//
// Other extensions stay silent; the frame is still persisted (the
// SystemMarker / persisted transcript path covers any future
// per-extension renderings).
func (a *Adapter) renderExtensionFrame(v *protocol.ExtensionFrame) {
	switch {
	case v.Payload.Extension == "liveview" && v.Payload.Op == "status":
		a.updateCompactorMarkerFlag(v.Payload.Data)
	case v.Payload.Extension == "compactor" && v.Payload.Op == "digest_set":
		a.renderCompactorDigestSet(v)
	}
}

// updateCompactorMarkerFlag inspects a liveview/status payload and
// mirrors its `extensions.compactor.ui_marker_enabled` field onto
// the adapter's atomic. A missing field is treated as "enabled"
// (default-on) per spec §11.7.
func (a *Adapter) updateCompactorMarkerFlag(data []byte) {
	if len(data) == 0 {
		return
	}
	var envelope struct {
		Extensions map[string]json.RawMessage `json:"extensions,omitempty"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return
	}
	raw, ok := envelope.Extensions["compactor"]
	if !ok || len(raw) == 0 {
		return
	}
	var c struct {
		UIMarkerEnabled bool `json:"ui_marker_enabled"`
	}
	c.UIMarkerEnabled = true // default-on if the field is absent
	if err := json.Unmarshal(raw, &c); err != nil {
		return
	}
	a.compactorMarkerDisabled.Store(!c.UIMarkerEnabled)
}

// renderCompactorDigestSet draws the transcript marker for a
// compactor digest_set frame. Suppressed when the operator's
// ui_marker.enabled flag is off (mirrored via
// updateCompactorMarkerFlag).
func (a *Adapter) renderCompactorDigestSet(v *protocol.ExtensionFrame) {
	if a.compactorMarkerDisabled.Load() {
		return
	}
	var p struct {
		Iteration   int `json:"iteration"`
		KeptCount   int `json:"kept_count"`
		BlocksCount int `json:"blocks_count"`
		// DigestPayload uses kept_verbatim + summary_blocks as
		// arrays — fall back to len() through nested decoding when
		// the dedicated count fields are absent (digest_set body
		// carries the raw DigestPayload, not the StatusPayload).
		KeptVerbatim  []json.RawMessage `json:"kept_verbatim"`
		SummaryBlocks []json.RawMessage `json:"summary_blocks"`
	}
	if err := json.Unmarshal(v.Payload.Data, &p); err != nil {
		return
	}
	msgCount := p.KeptCount
	if msgCount == 0 {
		msgCount = len(p.KeptVerbatim)
	}
	if msgCount == 0 {
		msgCount = p.BlocksCount
	}
	if msgCount == 0 {
		msgCount = len(p.SummaryBlocks)
	}
	if a.currentSection != "" {
		fmt.Fprintln(a.out)
		a.currentSection = ""
	}
	fmt.Fprintf(a.out,
		"─── history compacted (iter %d, %d msgs) ───\n",
		p.Iteration, msgCount)
	fmt.Fprint(a.out, "> ")
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
