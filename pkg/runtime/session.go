package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// Session is one long-lived conversation. Phase 1: one user, one
// agent. The session goroutine is started by Manager.spawn; clients
// only interact through Inbox / Outbox.
type Session struct {
	id      string
	agent   *Agent
	store   RuntimeStore
	models  *model.ModelRouter
	codec   *protocol.Codec
	cmds    *CommandRegistry
	notepad *Notepad
	logger  *slog.Logger

	// Per-session model overrides. /model use mutates this.
	overridesMu sync.RWMutex
	overrides   map[model.Intent]model.ModelSpec

	// Streaming state — set when a turn is in flight.
	inflightMu     sync.Mutex
	inflightCancel context.CancelFunc
	pendingSwitch  *modelSwitch

	// Materialisation state for restart-resume (Phase 4 fills this in).
	materialised atomic.Bool
	matOnce      sync.Once
	history      []model.Message

	in     chan protocol.Frame
	out    chan protocol.Frame
	closed atomic.Bool
}

// modelSwitch records a pending /model use until the next turn so
// the runtime can emit a system_marker on first use.
type modelSwitch struct {
	from model.ModelSpec
	to   model.ModelSpec
}

// SessionOption configures a Session at construction.
type SessionOption func(*Session)

// WithSessionLogger sets a per-session logger (useful in tests).
func WithSessionLogger(l *slog.Logger) SessionOption {
	return func(s *Session) { s.logger = l }
}

// NewSession constructs a Session bound to its dependencies.
func NewSession(
	id string,
	agent *Agent,
	store RuntimeStore,
	models *model.ModelRouter,
	cmds *CommandRegistry,
	codec *protocol.Codec,
	logger *slog.Logger,
	opts ...SessionOption,
) *Session {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Session{
		id:        id,
		agent:     agent,
		store:     store,
		models:    models,
		codec:     codec,
		cmds:      cmds,
		notepad:   NewNotepad(store, agent.ID(), id),
		logger:    logger,
		overrides: make(map[model.Intent]model.ModelSpec),
		in:        make(chan protocol.Frame, 16),
		out:       make(chan protocol.Frame, 32),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// ID returns the session identifier.
func (s *Session) ID() string { return s.id }

// Inbox is the channel callers push frames onto.
func (s *Session) Inbox() chan<- protocol.Frame { return s.in }

// Outbox is the read-only side adapters subscribe to.
func (s *Session) Outbox() <-chan protocol.Frame { return s.out }

// Notepad returns the session's notepad handle.
func (s *Session) Notepad() *Notepad { return s.notepad }

// SetModelOverride records a per-session model preference. The next
// turn will route through it and emit a system_marker.
func (s *Session) SetModelOverride(intent model.Intent, spec model.ModelSpec) {
	s.overridesMu.Lock()
	defer s.overridesMu.Unlock()
	prev, ok := s.overrides[intent]
	s.overrides[intent] = spec
	from := prev
	if !ok {
		// "from" is the runtime default for the intent.
		if def, defOk := s.models.SpecFor(intent); defOk {
			from = def
		}
	}
	s.pendingSwitch = &modelSwitch{from: from, to: spec}
}

func (s *Session) sessionModels() map[model.Intent]model.ModelSpec {
	s.overridesMu.RLock()
	defer s.overridesMu.RUnlock()
	if len(s.overrides) == 0 {
		return nil
	}
	out := make(map[model.Intent]model.ModelSpec, len(s.overrides))
	for k, v := range s.overrides {
		out[k] = v
	}
	return out
}

// emit persists a Frame and pushes it onto the Outbox. Persistence
// happens before delivery so observers can't see anything that
// failed to durably land. Emitting after the session has exited
// (Outbox closed) returns ErrSessionClosed instead of panicking.
func (s *Session) emit(ctx context.Context, f protocol.Frame) (err error) {
	if s.closed.Load() {
		return ErrSessionClosed
	}
	row, summary, perr := FrameToEventRow(f, s.agent.ID())
	if perr != nil {
		return fmt.Errorf("session %s: project frame: %w", s.id, perr)
	}
	if perr := s.store.AppendEvent(ctx, row, summary); perr != nil {
		return fmt.Errorf("session %s: persist frame: %w", s.id, perr)
	}
	defer func() {
		if r := recover(); r != nil {
			// Outbox was closed concurrently; treat as a graceful
			// shutdown signal rather than a crash.
			err = ErrSessionClosed
		}
	}()
	select {
	case s.out <- f:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Run drives the turn loop. Phase-2 skeleton: handles Cancel
// directly, routes SlashCommand through CommandRegistry, no LLM
// dispatch yet (that lands in Phase 3 / T037).
func (s *Session) Run(ctx context.Context) error {
	defer close(s.out)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case f, ok := <-s.in:
			if !ok {
				return nil
			}
			if err := s.handle(ctx, f); err != nil {
				s.logger.Error("session frame handler", "session", s.id, "err", err)
			}
		}
	}
}

// handle dispatches a single inbound Frame. The full LLM-call branch
// is wired in Phase 3 (T037); for Phase 2 the skeleton implements
// just enough to make the binary boot and exercise the persistence
// path for slash commands and lifecycle frames.
func (s *Session) handle(ctx context.Context, f protocol.Frame) error {
	switch v := f.(type) {
	case *protocol.Cancel:
		return s.handleCancel(ctx, v)
	case *protocol.SlashCommand:
		return s.handleSlashCommand(ctx, v)
	case *protocol.UserMessage:
		return s.handleUserMessage(ctx, v)
	case *protocol.SessionClosed:
		// Persist + emit, then mark closed and exit the Run loop.
		if err := s.emit(ctx, v); err != nil {
			return err
		}
		s.closed.Store(true)
		// Persist the status flip too.
		_ = s.store.UpdateSessionStatus(ctx, s.id, StatusClosed)
		// Closing the inbox unblocks Run's range and lets it return
		// cleanly; any in-flight Send to Inbox will see channel-closed.
		// We close it from inside the goroutine to avoid races with
		// concurrent Submit calls.
		go func() {
			defer func() { _ = recover() }()
			close(s.in)
		}()
		return nil
	default:
		// Other Frame kinds: persist and fan out unchanged.
		return s.emit(ctx, v)
	}
}

func (s *Session) handleCancel(ctx context.Context, f *protocol.Cancel) error {
	s.inflightMu.Lock()
	if s.inflightCancel != nil {
		s.inflightCancel()
	}
	s.inflightMu.Unlock()
	return s.emit(ctx, f)
}

func (s *Session) handleSlashCommand(ctx context.Context, f *protocol.SlashCommand) error {
	if err := s.emit(ctx, f); err != nil {
		return err
	}
	spec, ok := s.cmds.Lookup(f.Payload.Name)
	if !ok {
		errFrame := protocol.NewError(s.id, s.agent.Participant(), "unknown_command",
			fmt.Sprintf("no such command: /%s (try /help)", f.Payload.Name), false)
		return s.emit(ctx, errFrame)
	}
	env := CommandEnv{
		Session:     s,
		Author:      f.Author(),
		AgentAuthor: s.agent.Participant(),
		Models:      s.models,
		Notepad:     s.notepad,
		Logger:      s.logger,
		Description: spec.Description,
	}
	frames, err := spec.Handler(ctx, env, f.Payload.Args)
	if err != nil {
		errFrame := protocol.NewError(s.id, s.agent.Participant(), "command_error", err.Error(), true)
		return s.emit(ctx, errFrame)
	}
	for _, out := range frames {
		if err := s.emit(ctx, out); err != nil {
			return err
		}
	}
	return nil
}

// handleUserMessage runs one turn: persist the user input, hydrate
// the working window if needed, resolve a Model, stream chunks back
// out as Reasoning + AgentMessage frames, and emit a model_switched
// marker on the first turn after a /model use.
func (s *Session) handleUserMessage(ctx context.Context, f *protocol.UserMessage) error {
	if err := s.emit(ctx, f); err != nil {
		return err
	}
	if err := s.materialise(ctx); err != nil {
		s.logger.Warn("materialise failed; proceeding with empty history", "session", s.id, "err", err)
	}

	// If a /model use is pending, emit its marker before this turn.
	if err := s.emitPendingSwitch(ctx); err != nil {
		return err
	}

	// Build request: history + new user message.
	req := model.Request{
		Messages: append(append([]model.Message{}, s.history...),
			model.Message{Role: model.RoleUser, Content: f.Payload.Text}),
	}

	mdl, _, err := s.models.Resolve(ctx, model.Hint{
		Intent:        model.IntentDefault,
		SessionModels: s.sessionModels(),
	})
	if err != nil {
		errFrame := protocol.NewError(s.id, s.agent.Participant(),
			"model_unavailable", err.Error(), true)
		return s.emit(ctx, errFrame)
	}

	turnCtx, cancel := context.WithCancel(ctx)
	s.inflightMu.Lock()
	s.inflightCancel = cancel
	s.inflightMu.Unlock()
	defer func() {
		cancel()
		s.inflightMu.Lock()
		s.inflightCancel = nil
		s.inflightMu.Unlock()
	}()

	stream, err := mdl.Generate(turnCtx, req)
	if err != nil {
		errFrame := protocol.NewError(s.id, s.agent.Participant(),
			"model_call_failed", err.Error(), true)
		return s.emit(ctx, errFrame)
	}
	defer stream.Close()

	finalText, err := s.streamTurn(ctx, turnCtx, stream)
	if err != nil {
		// Distinguish cancellation from real errors.
		if turnCtx.Err() != nil && ctx.Err() == nil {
			// Cancellation by /cancel — already emitted by handleCancel.
			return nil
		}
		errFrame := protocol.NewError(s.id, s.agent.Participant(),
			"stream_error", err.Error(), true)
		_ = s.emit(ctx, errFrame)
		return err
	}

	// Append the new user + assistant message into the working window
	// so the next turn sees them without a re-walk.
	s.history = append(s.history, model.Message{Role: model.RoleUser, Content: f.Payload.Text})
	if finalText != "" {
		s.history = append(s.history, model.Message{Role: model.RoleAssistant, Content: finalText})
	}
	return nil
}

// streamTurn drains a Stream into Reasoning + AgentMessage frames.
// Returns the concatenated final assistant text for history.
//
// The end-of-stream final flag is set on the final-content chunk
// (chunk.Final=true) OR, if the provider didn't mark Final on a
// content chunk, on a synthetic close emit when the stream channel
// drains.
func (s *Session) streamTurn(ctx, turnCtx context.Context, stream model.Stream) (string, error) {
	agentSeq := 0
	reasoningSeq := 0
	var fullAnswer string
	var sawFinal bool
	for {
		chunk, more, err := stream.Next(turnCtx)
		if err != nil {
			return fullAnswer, err
		}
		if !more {
			break
		}
		if chunk.Reasoning != nil && *chunk.Reasoning != "" {
			rf := protocol.NewReasoning(s.id, s.agent.Participant(),
				*chunk.Reasoning, reasoningSeq, false)
			if err := s.emit(ctx, rf); err != nil {
				return fullAnswer, err
			}
			reasoningSeq++
		}
		if chunk.Content != nil && *chunk.Content != "" {
			fullAnswer += *chunk.Content
			af := protocol.NewAgentMessage(s.id, s.agent.Participant(),
				*chunk.Content, agentSeq, chunk.Final)
			if err := s.emit(ctx, af); err != nil {
				return fullAnswer, err
			}
			agentSeq++
			if chunk.Final {
				sawFinal = true
			}
		}
	}
	// Stream ended without an explicit final-flagged content chunk:
	// emit a zero-text closer so subscribers can detect the boundary.
	if agentSeq > 0 && !sawFinal {
		closer := protocol.NewAgentMessage(s.id, s.agent.Participant(),
			"", agentSeq, true)
		if err := s.emit(ctx, closer); err != nil {
			return fullAnswer, err
		}
	}
	return fullAnswer, nil
}

// emitPendingSwitch emits a system_marker for a queued /model use,
// then clears the flag. No-op if no switch is pending.
func (s *Session) emitPendingSwitch(ctx context.Context) error {
	s.inflightMu.Lock()
	switch_ := s.pendingSwitch
	s.pendingSwitch = nil
	s.inflightMu.Unlock()
	if switch_ == nil {
		return nil
	}
	marker := protocol.NewSystemMarker(s.id, s.agent.Participant(), "model_switched",
		map[string]any{"from": switch_.from.String(), "to": switch_.to.String()})
	return s.emit(ctx, marker)
}

// markClosed is called by the /end command after emitting
// session_closed. Updates the row status.
func (s *Session) markClosed(ctx context.Context) error {
	if err := s.store.UpdateSessionStatus(ctx, s.id, StatusClosed); err != nil {
		return err
	}
	s.closed.Store(true)
	return nil
}

// touchUpdated is used to refresh updated_at on activity. The hugr
// schema auto-bumps updated_at on UPDATE; we reuse UpdateSessionStatus
// with the same status to drive the touch. Phase 1 keeps this simple
// and accepts that a no-op UPDATE writes one round-trip per turn —
// trivial at the volumes phase 1 targets.
func (s *Session) touchUpdated(ctx context.Context) error {
	_ = ctx
	// Skipping for phase 1 — the engine bumps updated_at on every
	// row change including AppendEvent's (UPSERT) on hub.db. If the
	// schema doesn't auto-update, we still get an updated_at via the
	// next session_events insert. This is intentionally a no-op until
	// real-time presence telemetry needs it.
	return nil
}

// IsClosed reports whether the session has been closed.
func (s *Session) IsClosed() bool { return s.closed.Load() }

// LastActive returns time.Now (placeholder; Phase 4 fills this in
// from updated_at if needed).
func (s *Session) LastActive() time.Time { return time.Now().UTC() }
