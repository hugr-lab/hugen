package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/skill"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// Session is one long-lived conversation. Phase 1: one user, one
// agent. The session goroutine is started by Manager.spawn; clients
// only interact through Inbox / Outbox.
//
// Phase-4 tree-ctx-routing pivot (ADR
// `design/001-agent-runtime/phase-4-tree-ctx-routing.md`) extends
// Session with parent/children/depth/ctx/cancel/explicitTerminate so
// sub-agents can be owned by their parent Session rather than by
// Manager. The legacy fields (agent/store/models/codec/cmds/...)
// remain for back-compat with existing call sites; deps points at
// the same shared bundle so new constructors can inject everything
// in one go.
type Session struct {
	id           string
	depth        int                 // 0 for root; parent.depth+1 for subagent
	deps         *sessionDeps        // shared bundle; nil only in legacy NewSession callers
	agent        *Agent
	store        RuntimeStore
	models       *model.ModelRouter
	codec        *protocol.Codec
	cmds         *CommandRegistry
	notepad      *Notepad
	tools        *tool.ToolManager   // optional; nil disables tool dispatch
	skills       *skill.SkillManager // optional; consulted for per-skill max_turns
	maxToolIters int                 // 0 → defaultMaxToolIterations
	logger       *slog.Logger

	// Tree links (phase-4 pivot). parent is nil for root; children is
	// always non-nil so callers can lock+iterate without a nil-check.
	parent   *Session
	childMu  sync.Mutex
	children map[string]*Session

	// Per-session ctx + cancel. Set by newSession / newSessionRestore
	// before the goroutine launches; nil only for legacy NewSession
	// callers that haven't migrated. The goroutine reads ctx via the
	// argument to Run, so Session.ctx is just the canonical handle for
	// derivation in parent.Spawn (child.ctx = WithCancelCause(parent.ctx)).
	ctx    context.Context
	cancel context.CancelCauseFunc

	// openedAt mirrors the persisted SessionRow.CreatedAt (or, on
	// resume / restore, the original CreatedAt loaded from store).
	// Set by newSession / newSessionRestore so Manager.Open / Resume
	// can echo it back to the caller without an extra LoadSession.
	openedAt time.Time
	// explicitTerminate distinguishes "I called s.terminate(cause)"
	// from "my parent's ctx was cancelled and I'm cascading down".
	// Pivot 7 wires the read site in handleExit; pivot 2 just declares
	// the field.
	explicitTerminate atomic.Bool

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
	// done is closed by Run on exit. External callers (Manager.Terminate,
	// ShutdownAll) wait on it to know the session goroutine has
	// finished its exit handler — including any session_terminated
	// event append.
	done chan struct{}

	// terminate cancels the per-session ctx with a terminationCause.
	// Set by Manager.spawn; called from /end (cause carries
	// emitClose=false because the handler already emitted the
	// SessionClosed Frame for the transcript).
	//
	// Nil only for Sessions constructed directly by tests without a
	// Manager (rare); guards in handleSlashCommand check for nil.
	//
	// Phase-4 pivot: this is the public face of s.cancel — pivot 7
	// will wrap it with explicitTerminate flag set. For pivot 2, kept
	// as the bare CancelCauseFunc to preserve handleSlashCommand
	// call site without churn.
	terminate context.CancelCauseFunc
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
// model→tool→model loops. Precedence at runtime: per-skill
// metadata.hugen.max_turns (max across loaded skills) > this
// option > defaultMaxToolIterations. Useful when no skill is
// loaded but the deployment still wants a non-default ceiling.
func WithMaxToolIterations(n int) SessionOption {
	return func(s *Session) {
		if n > 0 {
			s.maxToolIters = n
		}
	}
}

// WithSkills attaches a SkillManager to the session so the Turn
// loop can read per-skill metadata (currently max_turns; phase-4
// adds sub-agent dispatch state). Optional — sessions without
// WithSkills behave the same as before, just without skill-driven
// caps.
func WithSkills(sm *skill.SkillManager) SessionOption {
	return func(s *Session) { s.skills = sm }
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
		done:      make(chan struct{}),
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

// Done returns a channel closed when the session goroutine has
// exited (terminal status persisted, outbox closed). External
// callers wait on this after pushing a Close intent into s.in.
func (s *Session) Done() <-chan struct{} { return s.done }

// Submit pushes a frame onto the session's inbox without crashing
// on a closed channel and without hanging on a full one. Three
// exit paths:
//
//   - ctx done → caller wants to bail out (shutdown timeout, API
//     cancel). Returns false so the caller can decide whether to
//     report or move on.
//   - Done closed → the session goroutine has exited; the frame
//     can never be delivered. Returns false.
//   - successful send → returns true.
//
// A "send on closed channel" panic is caught by recover so a race
// between the goroutine's exit defer and our send doesn't crash
// the process; the recovered case also maps to ok=false.
//
// External callers (Manager.Close, Manager.Suspend, ShutdownAll,
// adapters) use Submit instead of touching s.in directly so the
// "in writes belong to the session goroutine" invariant has a
// single, audit-friendly entry point.
func (s *Session) Submit(ctx context.Context, f protocol.Frame) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			ok = false
		}
	}()
	select {
	case s.in <- f:
		return true
	case <-s.done:
		return false
	case <-ctx.Done():
		return false
	}
}

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

// Run drives the turn loop. On ctx cancellation, the loop reads the
// cancel cause: if it's a *terminationCause (from Manager.Terminate
// or from /end via s.terminate), the goroutine appends a
// session_terminated event with the supplied reason and optionally
// emits a SessionClosed Frame for adapter back-compat. If the cancel
// has no cause (graceful process shutdown via Manager.ShutdownAll),
// the goroutine writes nothing — sessions without a terminal event
// are exactly the "needs-restart-decision" set on next boot
// (FR-028 + FR-029).
func (s *Session) Run(ctx context.Context) error {
	defer close(s.done) // signal external waiters BEFORE outbox closes
	defer close(s.out)
	for {
		select {
		case <-ctx.Done():
			s.handleExit(ctx)
			return ctx.Err()
		case f, ok := <-s.in:
			if !ok {
				return nil
			}
			if err := s.handle(ctx, f); err != nil {
				// Debug, not Error: handle() already emitted a
				// protocol.Error frame to the user where appropriate;
				// stderr Error here just clobbers the REPL prompt.
				s.logger.Debug("session frame handler", "session", s.id, "err", err)
			}
		}
	}
}

// handleExit runs when the per-session ctx fires. It distinguishes
// Manager.Terminate / /end (terminationCause attached) from graceful
// Manager.ShutdownAll (no cause): only the former path appends a
// session_terminated event.
//
// Persistence uses a fresh context.Background() because the session
// ctx is already cancelled at this point — the underlying store
// queries would fail to round-trip via a cancelled ctx. Persistence
// is bounded by an explicit deadline so a stuck store can't pin
// shutdown.
func (s *Session) handleExit(runCtx context.Context) {
	cause := context.Cause(runCtx)
	tc, ok := cause.(*terminationCause)
	if !ok {
		// Graceful shutdown — write nothing.
		return
	}
	if s.closed.Load() {
		return
	}
	s.closed.Store(true)
	persistCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	terminal := protocol.NewSessionTerminated(s.id, s.agent.Participant(), protocol.SessionTerminatedPayload{
		Reason: tc.reason,
	})
	if err := s.emit(persistCtx, terminal); err != nil {
		s.logger.Warn("session: append session_terminated", "session", s.id, "err", err)
	}
	if tc.emitClose {
		closed := protocol.NewSessionClosed(s.id, s.agent.Participant(), tc.reason)
		if err := s.emit(persistCtx, closed); err != nil {
			s.logger.Warn("session: emit SessionClosed marker", "session", s.id, "err", err)
		}
	}
}

// handle dispatches a single inbound Frame.
//
// Phase 4 removed the SessionClosed / SessionSuspended intent
// branches: lifecycle is now driven entirely through the per-session
// ctx (Manager.Terminate / s.terminate). SessionClosed Frames
// observed inbound (e.g., the legacy /end handler returning a
// SessionClosed frame) flow through the default emit path so adapters
// still see them in the transcript; the actual termination is
// triggered by handleSlashCommand calling s.terminate.
func (s *Session) handle(ctx context.Context, f protocol.Frame) error {
	switch v := f.(type) {
	case *protocol.Cancel:
		return s.handleCancel(ctx, v)
	case *protocol.SlashCommand:
		return s.handleSlashCommand(ctx, v)
	case *protocol.UserMessage:
		return s.handleUserMessage(ctx, v)
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
	var closeReason string
	for _, out := range frames {
		if c, ok := out.(*protocol.SessionClosed); ok {
			sawClose = true
			closeReason = c.Payload.Reason
		}
		if err := s.emit(ctx, out); err != nil {
			return err
		}
	}
	// If a handler emitted SessionClosed (e.g. /end), trigger the
	// per-session ctx-cancel via s.terminate so the Run loop's
	// handleExit appends a session_terminated event and exits.
	// emitClose=false: the handler already emitted the SessionClosed
	// Frame for the transcript; the exit handler must NOT emit a
	// duplicate.
	if sawClose && s.terminate != nil {
		s.terminate(&terminationCause{
			reason:    "user:" + f.Payload.Name + " " + closeReason,
			emitClose: false,
		})
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
	//
	// historyBaseline marks the index of this user message so we
	// can trim back to "before the failed turn" if the model call
	// dies without producing an assistant response. Without that
	// rollback the next user attempt would emit two consecutive
	// user-role messages — the comment further down ("Skipping
	// the assistant message — even when finalText is empty —
	// confuses providers") names the same failure mode for the
	// adjacent case. Applies to /cancel too: an aborted turn
	// leaves no assistant counterpart, so the user message has
	// to roll back as well.
	historyBaseline := len(s.history)
	s.history = append(s.history, model.Message{Role: model.RoleUser, Content: f.Payload.Text})

	cap := s.resolveToolIterCap(turnCtx)

	for iter := 0; iter < cap; iter++ {
		modelTools, err := s.modelToolsForSession(turnCtx)
		if err != nil {
			s.logger.Warn("session: build tool catalogue", "session", s.id, "err", err)
		}
		req := model.Request{
			Messages: s.buildMessages(turnCtx),
			Tools:    modelTools,
		}
		stream, err := mdl.Generate(turnCtx, req)
		if err != nil {
			s.history = s.history[:historyBaseline]
			errFrame := protocol.NewError(s.id, s.agent.Participant(),
				"model_call_failed", err.Error(), true)
			return s.emit(ctx, errFrame)
		}
		outcome, err := s.streamTurn(ctx, turnCtx, stream)
		_ = stream.Close()
		if err != nil {
			if turnCtx.Err() != nil && ctx.Err() == nil {
				s.history = s.history[:historyBaseline]
				return nil // /cancel — handleCancel already emitted.
			}
			s.history = s.history[:historyBaseline]
			errFrame := protocol.NewError(s.id, s.agent.Participant(),
				"stream_error", err.Error(), true)
			_ = s.emit(ctx, errFrame)
			return err
		}
		// Persist the assistant turn before the tool results so the
		// next model call sees well-formed history (assistant
		// requested → tool responded). Skipping the assistant
		// message — even when finalText is empty — confuses
		// providers that key tool results by their tool_call
		// antecedent (Gemma re-issues the call thinking it never
		// happened).
		if outcome.finalText != "" || len(outcome.toolCalls) > 0 {
			s.history = append(s.history, model.Message{
				Role:             model.RoleAssistant,
				Content:          outcome.finalText,
				ToolCalls:        outcome.toolCalls,
				Thinking:         outcome.thinking,
				ThoughtSignature: outcome.thoughtSignature,
			})
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

// resolveToolIterCap picks the tool-iteration cap for one user
// Turn. Precedence: loaded skills' max(metadata.hugen.max_turns)
// → session-level WithMaxToolIterations override → runtime
// default. Sampled once at the top of the user turn so the cap
// stays stable through the loop even if a tool call mutates the
// loaded skills mid-turn.
func (s *Session) resolveToolIterCap(ctx context.Context) int {
	if s.skills != nil {
		if b, err := s.skills.Bindings(ctx, s.id); err == nil && b.MaxTurns > 0 {
			return b.MaxTurns
		}
	}
	if s.maxToolIters > 0 {
		return s.maxToolIters
	}
	return defaultMaxToolIterations
}

// buildMessages prepends the per-Turn system message (agent
// constitution + concatenated skill instructions) to the chat
// history. Rebuilt every Turn because skill bindings can change
// between Turns (skill_load / skill_unload during a session). The
// returned slice is a fresh copy — callers can mutate it freely
// without touching s.history.
func (s *Session) buildMessages(ctx context.Context) []model.Message {
	out := make([]model.Message, 0, len(s.history)+1)
	if sys := s.systemPrompt(ctx); sys != "" {
		out = append(out, model.Message{Role: model.RoleSystem, Content: sys})
	}
	out = append(out, s.history...)
	return out
}

// systemPrompt assembles the system-prompt body for the next
// model.Generate call. Order:
//  1. Agent constitution (universal rules).
//  2. Body of every skill currently loaded into the session
//     (concrete tool-usage instructions for the active toolset).
//  3. Catalogue of every skill the agent can reach — both loaded
//     and unloaded — so the model picks the right one and calls
//     skill_load without a separate discovery tool round-trip.
//     Loaded skills are tagged so the model doesn't reload them.
func (s *Session) systemPrompt(ctx context.Context) string {
	var parts []string
	if s.agent != nil {
		if c := s.agent.Constitution(); c != "" {
			parts = append(parts, c)
		}
	}
	if s.skills != nil {
		if b, err := s.skills.Bindings(ctx, s.id); err == nil && b.Instructions != "" {
			parts = append(parts, b.Instructions)
		}
		if catalogue := s.skillCatalogue(ctx); catalogue != "" {
			parts = append(parts, catalogue)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n\n")
}

// skillCatalogue renders one bullet per skill in the store using
// the manifest's frontmatter `description` (capped at ~120 tokens
// by the manifest validator). Loaded skills carry a `(loaded)` tag
// so the model sees its current toolset alongside everything else
// available. Returns "" when the store is empty.
func (s *Session) skillCatalogue(ctx context.Context) string {
	all, err := s.skills.List(ctx)
	if err != nil || len(all) == 0 {
		return ""
	}
	loadedSet := map[string]struct{}{}
	for _, n := range s.skills.LoadedNames(ctx, s.id) {
		loadedSet[n] = struct{}{}
	}
	var b strings.Builder
	b.WriteString("## Available skills\n\nLoad any of these via the `skill_load` tool when their domain becomes relevant. Already-loaded skills are tagged `(loaded)`.\n\n")
	for _, sk := range all {
		b.WriteString("- `")
		b.WriteString(sk.Manifest.Name)
		b.WriteString("`")
		if _, on := loadedSet[sk.Manifest.Name]; on {
			b.WriteString(" (loaded)")
		}
		b.WriteString(" — ")
		b.WriteString(strings.TrimSpace(sk.Manifest.Description))
		b.WriteString("\n")
	}
	return b.String()
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
			Name:        tool.SanitizeName(t.Name),
			Description: t.Description,
			Schema:      schema,
		})
	}
	return out, nil
}

// turnOutcome carries everything one model.Generate call produced:
// the concatenated final assistant text, any tool calls the model
// emitted, plus the per-turn reasoning state (thinking text +
// thought_signature) some providers (Anthropic, Gemini 2.5+)
// require fed back on subsequent turns. handleUserMessage uses
// this to drive the bounded re-call loop that lets the LLM react
// to tool results.
type turnOutcome struct {
	finalText        string
	toolCalls        []model.ChunkToolCall
	thinking         string
	thoughtSignature string
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
		if chunk.Final {
			// Provider-supplied per-turn reasoning state. Captured
			// on the Final chunk only — earlier chunks carry deltas,
			// the finish event carries the canonical signature/
			// thinking blob.
			if chunk.Thinking != "" {
				out.thinking = chunk.Thinking
			}
			if chunk.ThoughtSignature != "" {
				out.thoughtSignature = chunk.ThoughtSignature
			}
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
// constructed without WithMaxToolIterations. 20 covers
// hugr-data exploration patterns where the model legitimately
// chains discovery → schema lookup → query validate → query
// without hitting the cap on a single user request.
const defaultMaxToolIterations = 20

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
	// Log the dispatch with full args BEFORE the call so the
	// operator can correlate any downstream slowdown / hang with
	// the exact request. Hash is included for phase-4
	// stuck-detection (deterministic id over name + raw args, see
	// pkg/models/hugr.go::hashToolCall). Sibling "tool result"
	// line follows after the call returns.
	s.logger.Debug("tool dispatch",
		"session", s.id, "tool", tc.Name, "hash", tc.Hash,
		"args", string(rawArgs))

	// Look up the Tool by fully-qualified name in the per-session
	// snapshot. The snapshot already filters by skill bindings so
	// an unbound provider won't appear here.
	snap, snapErr := s.tools.Snapshot(dispatchCtx, s.id)
	if snapErr != nil {
		s.logger.Warn("tool snapshot failed", "err", snapErr)
	}
	// Match on either the canonical name or the LLM-sanitized
	// form — providers see canonical "<provider>:<field>", but
	// the model receives (and echoes back) the dot/colon-stripped
	// shape. ToolManager.Dispatch uses theTool (canonical) below.
	var theTool tool.Tool
	for _, t := range snap.Tools {
		if t.Name == tc.Name || tool.SanitizeName(t.Name) == tc.Name {
			theTool = t
			break
		}
	}
	if theTool.Name == "" {
		// Surface the empty-snapshot case prominently — it usually
		// means a skill granted a wildcard pattern that doesn't
		// match the requested tool, or the provider hasn't returned
		// any tools yet. Without this log the dispatch line above
		// looks like a successful call followed by silence.
		available := make([]string, 0, len(snap.Tools))
		for _, t := range snap.Tools {
			available = append(available, t.Name)
		}
		s.logger.Warn("tool not in snapshot",
			"session", s.id, "tool", tc.Name,
			"snapshot_size", len(snap.Tools),
			"available", available)
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
		s.logger.Warn("tool result error",
			"session", s.id, "tool", tc.Name, "code", code, "err", err)
		s.emitToolError(ctx, tc.ID, tc.Name, code, err.Error(), "")
		return ""
	}
	// Log result AFTER the call so the operator sees the same
	// dispatch/result pairing in chronological order. Truncated
	// to 2 KiB so a big Parquet preview / file dump doesn't
	// drown the log; the full payload is in the tool_result frame.
	s.logger.Debug("tool result",
		"session", s.id, "tool", tc.Name,
		"result", truncatePayload(result, 2048))

	resultFrame := protocol.NewToolResult(s.id, s.agent.Participant(),
		tc.ID, json.RawMessage(result), false)
	if err := s.emit(ctx, resultFrame); err != nil {
		s.logger.Warn("emit tool_result", "err", err)
	}
	return string(result)
}

// truncatePayload caps a tool's raw JSON result for log lines so a
// large file dump or query response doesn't drown the log. Returns
// the head of the payload plus a "…(N bytes total)" suffix when
// truncated; passes the value through unchanged when it already
// fits.
func truncatePayload(b []byte, max int) string {
	if max <= 0 || len(b) <= max {
		return string(b)
	}
	return fmt.Sprintf("%s…(%d bytes total)", b[:max], len(b))
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

// IsClosed reports whether the session has been closed.
func (s *Session) IsClosed() bool { return s.closed.Load() }

// LastActive returns time.Now (placeholder; Phase 4 fills this in
// from updated_at if needed).
func (s *Session) LastActive() time.Time { return time.Now().UTC() }
