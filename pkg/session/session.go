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
	ownerID      string              // owner from SessionRow.OwnerID; inherited by subagents
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
	// Set by s.terminate() before s.cancel() fires; read by
	// handleExit to pick the session_terminated reason:
	//
	//   - explicitTerminate=true  → write tc.reason verbatim ("user:/end",
	//     "subagent_cancel: ...", "completed", etc.)
	//   - explicitTerminate=false → write protocol.TerminationCancelCascade
	//     (the parent's terminate caused our ctx to fire).
	explicitTerminate atomic.Bool

	// Per-session model overrides. /model use mutates this. The
	// overridesMu lock survives the C5 select-loop refactor because
	// SetModelOverride is reachable from CommandEnv handlers and from
	// future Tool implementations that may run on background goroutines
	// (e.g. Manager-as-ToolProvider in C7).
	overridesMu sync.RWMutex
	overrides   map[model.Intent]model.ModelSpec

	// pendingSwitch captures a /model use queued for the next turn.
	// Single-goroutine (Run) reader/writer post-C5 — no lock needed.
	pendingSwitch *modelSwitch

	// Per-turn state populated by startTurn (turn.go). All four fields
	// are owned by the Run goroutine and reset to nil in retireTurn:
	//   - turnCtx / turnCancel: child of runCtx; cancelled by /cancel
	//     so the model + tool dispatch goroutines abort cleanly.
	//   - modelChunks: fan-in from the model goroutine. nil between
	//     turns and after the goroutine closes the channel — select
	//     case on a nil channel blocks forever, disabling the branch.
	//   - toolResults: same pattern for the tool dispatcher.
	turnCtx     context.Context
	turnCancel  context.CancelFunc
	modelChunks chan modelChunkEvent
	toolResults chan toolResultEvent
	turnState   *turnState

	// turnWG tracks per-turn goroutines (model streamer + tool
	// dispatcher) so Run can wait for them to exit before closing
	// s.out. Without this wait the dispatcher's emit races with
	// Run's defer close(s.out) — visible immediately to the race
	// detector even when defer-recover masks the runtime panic.
	turnWG sync.WaitGroup

	// pendingInbound buffers RouteBuffered Frames received mid-turn.
	// Drained at every turn boundary into s.history. C5 buffers
	// non-Cancel/non-SlashCommand frames received while a turn is in
	// flight; C6 will replace the inline routing with a kindRoutes
	// table.
	pendingInbound []protocol.Frame

	// activeToolFeed is the slot a phase-4 blocking system tool (e.g.
	// wait_subagents) registers to consume RouteToolFeed Frames. The
	// Run goroutine reads via Load on every routeInbound call; the tool
	// dispatcher (a different goroutine) writes via Store / clears via
	// Store(nil). atomic.Pointer keeps the read+write race-free without
	// adding a mutex hot-path on every inbound frame.
	activeToolFeed atomic.Pointer[ToolFeed]

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

}

// terminate is the explicit-cancel path: called from Manager.Terminate
// and from handleSlashCommand on /end. It sets explicitTerminate=true
// before cancelling so handleExit knows to write the caller's reason
// verbatim instead of falling back to "cancel_cascade".
//
// No-op for Sessions constructed via legacy NewSession that haven't
// run buildSessionShell (no s.cancel wired up). Idempotent: a second
// call after the ctx is already cancelled is harmless — the first
// cause wins per context semantics, and explicitTerminate just stays
// true.
func (s *Session) terminate(cause *terminationCause) {
	if s == nil || s.cancel == nil {
		return
	}
	s.explicitTerminate.Store(true)
	s.cancel(cause)
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

// Run drives the event-driven session loop (phase-4 spec §10.3).
// Three live event sources select against ctx.Done:
//
//   - s.in: inbound Frames. Routed inline (Cancel / SlashCommand /
//     UserMessage) or buffered into pendingInbound for drain at the
//     next turn boundary.
//   - s.modelChunks: per-chunk events from a running model.Generate
//     goroutine. nil between turns — a select case on a nil channel
//     blocks forever, so the branch is automatically disabled.
//   - s.toolResults: tool dispatch results from a running tool
//     dispatcher goroutine. Same nil-channel pattern.
//
// After every select branch, advanceOrFinish runs if turnComplete()
// reports the model + tool dispatchers have both exited. That's the
// turn boundary where pendingInbound drains, the next iteration kicks
// off, or the turn retires (FR-014).
//
// Termination cases (unchanged from C4):
//   - graceful (rootCancel without cause): handleExit writes nothing.
//   - explicit (s.terminate): handleExit appends session_terminated
//     with the caller's reason.
//   - cascade (parent terminated): handleExit appends with reason
//     "cancel_cascade".
func (s *Session) Run(ctx context.Context) error {
	defer close(s.done) // signal external waiters BEFORE outbox closes
	defer close(s.out)
	for {
		select {
		case <-ctx.Done():
			// Cancel any in-flight turn so the model + tool goroutines
			// see turnCtx.Done and exit, then wait for them. Without
			// the wait, dispatcher emits race with the deferred
			// close(s.out) below; -race flags it even though
			// defer-recover masks the runtime panic.
			if s.turnCancel != nil {
				s.turnCancel()
			}
			s.waitTurnGoroutines(5 * time.Second)
			s.handleExit(ctx)
			return ctx.Err()
		case f, ok := <-s.in:
			if !ok {
				return nil
			}
			if err := s.routeInbound(ctx, f); err != nil {
				s.logger.Debug("session frame handler", "session", s.id, "err", err)
			}
		case ev, ok := <-s.modelChunks:
			if !ok {
				s.modelChunks = nil
			} else {
				s.handleModelEvent(ctx, ev)
			}
		case ev, ok := <-s.toolResults:
			if !ok {
				s.toolResults = nil
			} else {
				s.handleToolResult(ctx, ev)
			}
		}
		if s.turnComplete() {
			s.advanceOrFinish(ctx)
		}
	}
}

// handleExit runs when the per-session ctx fires. Three cases:
//
//   - graceful (Manager.ShutdownAll → rootCancel with no cause):
//     ctx.Cause is nil. Write nothing — phase-4 promise (FR-028 +
//     FR-029) is "no terminal event ⇒ this session is the
//     restart-walker's responsibility on next boot".
//
//   - explicit (s.terminate(cause) — from Manager.Terminate or /end):
//     explicitTerminate=true, ctx.Cause is *terminationCause. Write
//     session_terminated{tc.reason}; optionally emit SessionClosed
//     for adapter back-compat (tc.emitClose=true).
//
//   - cascade (parent's ctx was cancelled with cause; ours derives
//     from parent.ctx so we see the same cause but never set our
//     own explicit flag): explicitTerminate=false, ctx.Cause is
//     *terminationCause. Write session_terminated{cancel_cascade}
//     and suppress SessionClosed — cascade is an internal lifecycle
//     event, not a transcript message.
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
	reason := tc.reason
	emitClose := tc.emitClose
	if !s.explicitTerminate.Load() {
		// Cascade from a terminated parent. Override the inherited
		// cause so the persisted reason names the actual mechanism.
		reason = protocol.TerminationCancelCascade
		emitClose = false
	}
	persistCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Persist session_terminated directly through the store: emit()
	// short-circuits on s.closed (guarding against post-exit writes
	// from racing handlers), but the terminal event IS the close
	// signal — it has to land before we set s.closed=true. After this
	// point any concurrent emit returns ErrSessionClosed cleanly.
	terminal := protocol.NewSessionTerminated(s.id, s.agent.Participant(), protocol.SessionTerminatedPayload{
		Reason: reason,
	})
	if termRow, summary, perr := FrameToEventRow(terminal, s.agent.ID()); perr == nil {
		if nextSeq, serr := s.store.NextSeq(persistCtx, s.id); serr == nil {
			termRow.Seq = nextSeq
			if setter, ok := any(terminal).(protocol.SeqSetter); ok {
				setter.SetSeq(nextSeq)
			}
		}
		if err := s.store.AppendEvent(persistCtx, termRow, summary); err != nil {
			s.logger.Warn("session: append session_terminated", "session", s.id, "err", err)
		}
	} else {
		s.logger.Warn("session: project session_terminated", "session", s.id, "err", perr)
	}
	s.closed.Store(true)
	// SessionClosed is the model-/adapter-visible counterpart;
	// best-effort outbox push is fine since closed=true is now set
	// and emit will recover-safely panic on a closed outbox.
	if emitClose {
		closed := protocol.NewSessionClosed(s.id, s.agent.Participant(), reason)
		if cRow, cSum, perr := FrameToEventRow(closed, s.agent.ID()); perr == nil {
			if nextSeq, serr := s.store.NextSeq(persistCtx, s.id); serr == nil {
				cRow.Seq = nextSeq
				if setter, ok := any(closed).(protocol.SeqSetter); ok {
					setter.SetSeq(nextSeq)
				}
			}
			if err := s.store.AppendEvent(persistCtx, cRow, cSum); err != nil {
				s.logger.Warn("session: append session_closed", "session", s.id, "err", err)
			}
		}
		// Outbox push: defer-recover on a closed outbox.
		func() {
			defer func() { _ = recover() }()
			select {
			case s.out <- closed:
			default:
			}
		}()
	}
}

// routeInbound dispatches a single inbound Frame.
//
// Control frames (Cancel, SlashCommand, UserMessage) handle inline:
// they're the session-lifecycle triggers, not session-to-session
// data, and the phase-4 three-route model (§10.2) targets multi-
// session frames (subagent_*, whiteboard_*, future hitl_*).
//
// Everything else routes through routeFor (pkg/session/routes.go):
//   - RouteInternal → dispatchInternal runs a sync side-effect
//     handler from internalHandlers; the Frame never reaches
//     s.history. C6 ships an empty handler table — phase-4 step
//     10 (whiteboard primitive) fills it in.
//   - RouteToolFeed → if s.activeToolFeed is registered AND its
//     Consumes predicate matches the kind, the Frame is forwarded
//     to the blocking tool's feed; otherwise falls back to
//     RouteBuffered.
//   - RouteBuffered (default) → if a turn is in flight, append to
//     pendingInbound for drain at the next boundary; if idle, emit
//     pass-through so transcript stays consistent (no turn means
//     no boundary to drain into).
//
// SessionClosed Frames observed inbound (e.g. legacy /end handler
// returning a SessionClosed frame directly) flow through the buffered
// path so adapters still see them in the transcript; the actual
// termination is triggered by handleSlashCommand calling s.terminate.
func (s *Session) routeInbound(ctx context.Context, f protocol.Frame) error {
	switch v := f.(type) {
	case *protocol.Cancel:
		return s.handleCancel(ctx, v)
	case *protocol.SlashCommand:
		return s.handleSlashCommand(ctx, v)
	case *protocol.UserMessage:
		// Concurrent UserMessage during a turn is unusual (UI typically
		// gates on AgentMessage{Final:true}). Buffer it; advanceOrFinish
		// will fold it into history at the next turn boundary so the
		// next prompt sees the late input.
		if s.turnState != nil {
			s.pendingInbound = append(s.pendingInbound, f)
			return nil
		}
		s.startTurn(ctx, v)
		return nil
	}

	switch routeFor(f.Kind()) {
	case RouteInternal:
		s.dispatchInternal(ctx, f)
		return nil
	case RouteToolFeed:
		if feed := s.activeToolFeed.Load(); feed != nil &&
			feed.Consumes != nil && feed.Consumes(f.Kind()) {
			feed.Feed(f)
			return nil
		}
		// No matching feed: fall through to RouteBuffered.
		fallthrough
	case RouteBuffered:
		if s.turnState == nil {
			return s.emit(ctx, f)
		}
		s.pendingInbound = append(s.pendingInbound, f)
		return nil
	}
	return nil
}

// handleCancel aborts the in-flight turn (if any) by cancelling
// turnCtx — the model and tool dispatcher goroutines will see
// turnCtx.Done, exit, and close their fan-in channels. The Run loop
// then sees ok=false, nils s.modelChunks / s.toolResults, and
// turnComplete() returns true on the next pass; advanceOrFinish
// rolls back history baseline (nothing is emitted to the user — the
// Cancel frame itself is the user-visible signal).
//
// Cascade=true (`/cancel all`): in addition to the local turn abort,
// terminate every active child with reason "cancel_cascade". Each
// child's ctx is derived from this session's ctx, so the cause
// propagates down the entire subtree without a fan-out walk here.
// The receiving session itself does NOT terminate — only its
// turn aborts (per phase-4-spec §13.2 #5).
func (s *Session) handleCancel(ctx context.Context, f *protocol.Cancel) error {
	if s.turnCancel != nil {
		s.turnCancel()
	}
	if f.Payload.Cascade {
		s.cascadeCancelChildren()
	}
	return s.emit(ctx, f)
}

// cascadeCancelChildren walks the immediate children map and triggers
// terminate on each. Idempotent on already-closed children. A child
// that's mid-spawn (registered but goroutine not yet running its first
// select) sees turnCtx.Done immediately on entry into Run.
func (s *Session) cascadeCancelChildren() {
	s.childMu.Lock()
	children := make([]*Session, 0, len(s.children))
	for _, c := range s.children {
		children = append(children, c)
	}
	s.childMu.Unlock()
	for _, c := range children {
		if c.IsClosed() {
			continue
		}
		c.terminate(&terminationCause{
			reason:    protocol.TerminationCancelCascade,
			emitClose: false,
		})
	}
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
	if sawClose {
		s.terminate(&terminationCause{
			reason:    "user:" + f.Payload.Name + " " + closeReason,
			emitClose: false,
		})
	}
	return nil
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
//
// Two contexts:
//   - dispatchCtx (turnCtx): threaded into permission resolve and
//     tool.Dispatch so /cancel cleanly aborts long-running tools.
//   - emitCtx (runCtx): used for s.emit so transcript frames keep
//     landing even if the user is mid-cancellation; emit's own ctx
//     is the session's run ctx, never cancelled until process shutdown.
func (s *Session) dispatchToolCall(turnCtx, emitCtx context.Context, tc model.ChunkToolCall) string {
	if s.tools == nil {
		s.emitToolError(emitCtx, tc.ID, tc.Name, protocol.ToolErrorNotFound,
			"tool dispatch not configured for this session", "")
		return ""
	}
	dispatchCtx := perm.WithSession(turnCtx, perm.SessionContext{SessionID: s.id})
	// Wire the live *Session into the dispatch ctx so session-scoped
	// ToolProviders (Manager-as-ToolProvider, skill_files, …) can
	// recover the caller without going through Manager.Get — Manager
	// is root-only after pivot 4, so a sub-agent caller would not be
	// findable that way.
	dispatchCtx = WithSession(dispatchCtx, s)

	rawArgs := marshalToolArgs(tc.Args)
	callFrame := protocol.NewToolCall(s.id, s.agent.Participant(), tc.ID, tc.Name, tc.Args)
	if err := s.emit(emitCtx, callFrame); err != nil {
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
		s.emitToolError(emitCtx, tc.ID, tc.Name, protocol.ToolErrorNotFound,
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
			s.emitToolError(emitCtx, tc.ID, tc.Name, protocol.ToolErrorPermissionDenied,
				fmt.Sprintf("tool %q denied by %s tier", tc.Name, tier), tier)
			s.emitToolDeniedMarker(emitCtx, tc.Name, tier)
			return ""
		}
		s.emitToolError(emitCtx, tc.ID, tc.Name, "io", err.Error(), "")
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
		s.emitToolError(emitCtx, tc.ID, tc.Name, code, err.Error(), "")
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
	if err := s.emit(emitCtx, resultFrame); err != nil {
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
// then clears the flag. No-op if no switch is pending. Single-goroutine
// (Run) reader/writer post-C5 — no lock needed.
func (s *Session) emitPendingSwitch(ctx context.Context) error {
	switch_ := s.pendingSwitch
	s.pendingSwitch = nil
	if switch_ == nil {
		return nil
	}
	marker := protocol.NewSystemMarker(s.id, s.agent.Participant(), "model_switched",
		map[string]any{"from": switch_.from.String(), "to": switch_.to.String()})
	return s.emit(ctx, marker)
}

// waitTurnGoroutines blocks until all per-turn goroutines (model
// streamer + tool dispatcher) finish, or the timeout elapses.
// Bounded so a stuck tool can't pin shutdown — beyond the deadline
// we drop into the close path with goroutines still running, and
// their writes hit defer-recover on a closed s.out.
func (s *Session) waitTurnGoroutines(timeout time.Duration) {
	if timeout <= 0 {
		s.turnWG.Wait()
		return
	}
	done := make(chan struct{})
	go func() {
		s.turnWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		s.logger.Warn("session: turn goroutines did not exit within deadline",
			"session", s.id, "deadline", timeout)
	}
}

// IsClosed reports whether the session has been closed.
func (s *Session) IsClosed() bool { return s.closed.Load() }

// LastActive returns time.Now (placeholder; Phase 4 fills this in
// from updated_at if needed).
func (s *Session) LastActive() time.Time { return time.Now().UTC() }
