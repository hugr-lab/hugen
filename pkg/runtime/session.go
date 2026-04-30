package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/tool"
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
	tools        *tool.ToolManager // optional; nil disables tool dispatch
	maxToolIters int               // 0 → defaultMaxToolIterations
	logger       *slog.Logger

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

// WithTools attaches a ToolManager to the session so the Turn loop
// can dispatch model-emitted tool calls. Sessions constructed
// without WithTools simply skip the tool dispatch branch — the
// model can stream its tool_call chunks but they're surfaced as
// tool_error{code: not_found} so the LLM gets a clean signal.
func WithTools(tm *tool.ToolManager) SessionOption {
	return func(s *Session) { s.tools = tm }
}

// WithMaxToolIterations overrides the per-Turn cap on
// model→tool→model loops (default 15, mirroring ADK's
// defaultDispatchMaxTurns). Caps are useful guardrails against
// runaway tool loops on weak models; configurable per deployment
// because data-exploration scenarios (hugr explorer, multi-step
// reasoning) routinely need more.
func WithMaxToolIterations(n int) SessionOption {
	return func(s *Session) {
		if n > 0 {
			s.maxToolIters = n
		}
	}
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
	// Allocate the seq cursor BEFORE AppendEvent so the in-memory
	// Frame can be tagged with its seq atomically with persistence.
	// Session.Run is a single goroutine per session, so NextSeq +
	// AppendEvent + outbox push has no within-session race; the
	// per-session seq column has a uniqueness invariant the store
	// upholds across sessions.
	nextSeq, perr := s.store.NextSeq(ctx, s.id)
	if perr != nil {
		return fmt.Errorf("session %s: next seq: %w", s.id, perr)
	}
	row.Seq = nextSeq
	if setter, ok := f.(protocol.SeqSetter); ok {
		setter.SetSeq(nextSeq)
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
		// SessionClosed never arrives inbound in phase 1 — the agent
		// emits it itself via the /end handler, where it's caught
		// inside handleSlashCommand. If it ever does, treat it as a
		// passive frame: persist + fan out, no lifecycle effect.
		return s.emit(ctx, v)
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
	var sawClose bool
	for _, out := range frames {
		if _, ok := out.(*protocol.SessionClosed); ok {
			sawClose = true
		}
		if err := s.emit(ctx, out); err != nil {
			return err
		}
	}
	// If a handler emitted SessionClosed, persist status=closed and
	// stop the loop. We close s.in from a side goroutine to avoid
	// racing concurrent Submit calls; the recover guards against a
	// double close from ShutdownAll.
	if sawClose {
		if err := s.MarkClosed(ctx); err != nil {
			s.logger.Warn("session: MarkClosed", "session", s.id, "err", err)
		}
		go func() {
			defer func() { _ = recover() }()
			close(s.in)
		}()
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

	// First model turn: the user's input is the trailing message.
	// Subsequent iterations append assistant + tool messages from
	// dispatched tool calls so the LLM can react.
	s.history = append(s.history, model.Message{Role: model.RoleUser, Content: f.Payload.Text})

	cap := s.maxToolIters
	if cap <= 0 {
		cap = defaultMaxToolIterations
	}

	for iter := 0; iter < cap; iter++ {
		modelTools, err := s.modelToolsForSession(turnCtx)
		if err != nil {
			s.logger.Warn("session: build tool catalogue", "session", s.id, "err", err)
		}
		req := model.Request{
			Messages: append([]model.Message{}, s.history...),
			Tools:    modelTools,
		}
		stream, err := mdl.Generate(turnCtx, req)
		if err != nil {
			errFrame := protocol.NewError(s.id, s.agent.Participant(),
				"model_call_failed", err.Error(), true)
			return s.emit(ctx, errFrame)
		}
		outcome, err := s.streamTurn(ctx, turnCtx, stream)
		_ = stream.Close()
		if err != nil {
			if turnCtx.Err() != nil && ctx.Err() == nil {
				return nil // /cancel — handleCancel already emitted.
			}
			errFrame := protocol.NewError(s.id, s.agent.Participant(),
				"stream_error", err.Error(), true)
			_ = s.emit(ctx, errFrame)
			return err
		}
		if outcome.finalText != "" {
			s.history = append(s.history, model.Message{Role: model.RoleAssistant, Content: outcome.finalText})
		}
		if len(outcome.toolCalls) == 0 {
			return nil
		}
		for _, tc := range outcome.toolCalls {
			result := s.dispatchToolCall(ctx, tc)
			s.history = append(s.history, model.Message{
				Role:       model.RoleTool,
				Content:    result,
				ToolCallID: tc.ID,
			})
		}
	}
	s.logger.Warn("session: tool re-call cap hit", "session", s.id, "max", cap)
	limitFrame := protocol.NewError(s.id, s.agent.Participant(),
		"tool_iteration_limit", fmt.Sprintf("max tool re-call iterations (%d) reached", cap), false)
	return s.emit(ctx, limitFrame)
}

// modelToolsForSession converts the per-session ToolManager
// snapshot into the []model.Tool catalogue the LLM provider
// receives. Returns an empty slice when the session has no
// ToolManager wired.
func (s *Session) modelToolsForSession(ctx context.Context) ([]model.Tool, error) {
	if s.tools == nil {
		return nil, nil
	}
	snap, err := s.tools.Snapshot(ctx, s.id)
	if err != nil {
		return nil, err
	}
	out := make([]model.Tool, 0, len(snap.Tools))
	for _, t := range snap.Tools {
		var schema map[string]any
		if len(t.ArgSchema) > 0 {
			if err := json.Unmarshal(t.ArgSchema, &schema); err != nil {
				s.logger.Warn("session: bad tool arg schema",
					"session", s.id, "tool", t.Name, "err", err)
				schema = nil
			}
		}
		out = append(out, model.Tool{
			Name:        t.Name,
			Description: t.Description,
			Schema:      schema,
		})
	}
	return out, nil
}

// turnOutcome carries everything one model.Generate call produced:
// the concatenated final assistant text and any tool calls the
// model emitted. handleUserMessage uses this to drive the bounded
// re-call loop that lets the LLM react to tool results.
type turnOutcome struct {
	finalText string
	toolCalls []model.ChunkToolCall
}

// streamTurn drains a Stream into Reasoning + AgentMessage frames
// and collects every tool call the model emitted. Tool dispatch
// itself happens after the stream drains (in handleUserMessage)
// so the entire content/reasoning stream lands in the transcript
// before any tool plumbing.
//
// The end-of-stream final flag is set on the final-content chunk
// (chunk.Final=true) OR, if the provider didn't mark Final on a
// content chunk, on a synthetic close emit when the stream channel
// drains.
func (s *Session) streamTurn(ctx, turnCtx context.Context, stream model.Stream) (turnOutcome, error) {
	agentSeq := 0
	reasoningSeq := 0
	out := turnOutcome{}
	var sawFinal bool
	for {
		chunk, more, err := stream.Next(turnCtx)
		if err != nil {
			return out, err
		}
		if !more {
			break
		}
		if chunk.Reasoning != nil && *chunk.Reasoning != "" {
			rf := protocol.NewReasoning(s.id, s.agent.Participant(),
				*chunk.Reasoning, reasoningSeq, false)
			if err := s.emit(ctx, rf); err != nil {
				return out, err
			}
			reasoningSeq++
		}
		if chunk.Content != nil && *chunk.Content != "" {
			out.finalText += *chunk.Content
			af := protocol.NewAgentMessage(s.id, s.agent.Participant(),
				*chunk.Content, agentSeq, chunk.Final)
			if err := s.emit(ctx, af); err != nil {
				return out, err
			}
			agentSeq++
			if chunk.Final {
				sawFinal = true
			}
		}
		if chunk.ToolCall != nil {
			out.toolCalls = append(out.toolCalls, *chunk.ToolCall)
		}
	}
	// Stream ended without an explicit final-flagged content chunk:
	// emit a zero-text closer so subscribers can detect the boundary.
	// Skipped when the stream produced only tool calls — there's
	// another model turn coming.
	if agentSeq > 0 && !sawFinal && len(out.toolCalls) == 0 {
		closer := protocol.NewAgentMessage(s.id, s.agent.Participant(),
			"", agentSeq, true)
		if err := s.emit(ctx, closer); err != nil {
			return out, err
		}
	}
	return out, nil
}

// defaultMaxToolIterations is the per-Turn cap on
// model→tool→model loops applied when the session was
// constructed without WithMaxToolIterations. 15 mirrors ADK's
// defaultDispatchMaxTurns — proven adequate for typical hugr-
// explorer / multi-step-reasoning sessions.
const defaultMaxToolIterations = 15

// dispatchToolCall handles one model-emitted tool call: emits the
// tool_call frame, runs Tier-1 permission resolution, and either
// dispatches the call (success path) or surfaces a tool_error
// frame plus a tool_denied marker (deny path). Returns the JSON
// payload that should be fed back to the model as a tool-role
// message; "" when the dispatch failed and there's nothing
// useful to feed back beyond the error already on the wire.
func (s *Session) dispatchToolCall(ctx context.Context, tc model.ChunkToolCall) string {
	if s.tools == nil {
		s.emitToolError(ctx, tc.ID, tc.Name, protocol.ToolErrorNotFound,
			"tool dispatch not configured for this session", "")
		return ""
	}
	dispatchCtx := perm.WithSession(ctx, perm.SessionContext{SessionID: s.id})

	rawArgs := marshalToolArgs(tc.Args)
	callFrame := protocol.NewToolCall(s.id, s.agent.Participant(), tc.ID, tc.Name, tc.Args)
	if err := s.emit(ctx, callFrame); err != nil {
		s.logger.Warn("emit tool_call", "err", err)
	}

	// Look up the Tool by fully-qualified name in the per-session
	// snapshot. The snapshot already filters by skill bindings so
	// an unbound provider won't appear here.
	snap, snapErr := s.tools.Snapshot(dispatchCtx, s.id)
	if snapErr != nil {
		s.logger.Warn("tool snapshot failed", "err", snapErr)
	}
	var theTool tool.Tool
	for _, t := range snap.Tools {
		if t.Name == tc.Name {
			theTool = t
			break
		}
	}
	if theTool.Name == "" {
		s.emitToolError(ctx, tc.ID, tc.Name, protocol.ToolErrorNotFound,
			fmt.Sprintf("tool %q not in current snapshot", tc.Name), "")
		return ""
	}

	p, effective, err := s.tools.Resolve(dispatchCtx, theTool, rawArgs)
	if err != nil {
		if errors.Is(err, tool.ErrPermissionDenied) {
			tier := "config"
			if p.FromUser {
				tier = "user"
			} else if p.FromRemote {
				tier = "remote"
			}
			s.emitToolError(ctx, tc.ID, tc.Name, protocol.ToolErrorPermissionDenied,
				fmt.Sprintf("tool %q denied by %s tier", tc.Name, tier), tier)
			s.emitToolDeniedMarker(ctx, tc.Name, tier)
			return ""
		}
		s.emitToolError(ctx, tc.ID, tc.Name, "io", err.Error(), "")
		return ""
	}

	result, err := s.tools.Dispatch(dispatchCtx, theTool, effective)
	if err != nil {
		code := "io"
		switch {
		case errors.Is(err, tool.ErrUnknownProvider), errors.Is(err, tool.ErrUnknownTool):
			code = protocol.ToolErrorNotFound
		case errors.Is(err, tool.ErrProviderRemoved):
			code = protocol.ToolErrorProviderRemoved
		}
		s.emitToolError(ctx, tc.ID, tc.Name, code, err.Error(), "")
		return ""
	}

	resultFrame := protocol.NewToolResult(s.id, s.agent.Participant(),
		tc.ID, json.RawMessage(result), false)
	if err := s.emit(ctx, resultFrame); err != nil {
		s.logger.Warn("emit tool_result", "err", err)
	}
	return string(result)
}

func (s *Session) emitToolError(ctx context.Context, toolID, name, code, msg, tier string) {
	payload := protocol.ToolError{Code: code, Message: msg, Tier: tier}
	frame := protocol.NewToolResult(s.id, s.agent.Participant(), toolID, payload, true)
	if err := s.emit(ctx, frame); err != nil {
		s.logger.Warn("emit tool_error", "err", err, "tool", name)
	}
}

func (s *Session) emitToolDeniedMarker(ctx context.Context, name, tier string) {
	mk := protocol.NewSystemMarker(s.id, s.agent.Participant(),
		protocol.SubjectToolDenied,
		map[string]any{"tool": name, "tier": tier})
	if err := s.emit(ctx, mk); err != nil {
		s.logger.Warn("emit tool_denied marker", "err", err, "tool", name)
	}
}

// marshalToolArgs encodes the model-supplied args (typically
// map[string]any after JSON unmarshal in the provider) back to
// JSON. Already-RawMessage values pass through.
func marshalToolArgs(args any) json.RawMessage {
	if args == nil {
		return json.RawMessage(`{}`)
	}
	if raw, ok := args.(json.RawMessage); ok {
		return raw
	}
	if s, ok := args.(string); ok {
		// Some providers stream args as a JSON string.
		return json.RawMessage(s)
	}
	body, err := json.Marshal(args)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(body)
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

// MarkClosed flips the session status to closed and sets the
// in-memory closed flag. Called by the built-in /end handler.
// Idempotent: a second call is a no-op once the row is already
// closed (status update is idempotent at the store level too).
func (s *Session) MarkClosed(ctx context.Context) error {
	if err := s.store.UpdateSessionStatus(ctx, s.id, StatusClosed); err != nil {
		return fmt.Errorf("session %s: mark closed: %w", s.id, err)
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
