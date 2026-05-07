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
	"github.com/hugr-lab/hugen/pkg/session/store"
	"github.com/hugr-lab/hugen/pkg/session/tools/notepad"
	"github.com/hugr-lab/hugen/pkg/session/tools/plan"
	"github.com/hugr-lab/hugen/pkg/session/tools/whiteboard"
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
	id               string
	ownerID          string // owner from SessionRow.OwnerID; inherited by subagents
	depth            int    // 0 for root; parent.depth+1 for subagent
	deps             *Deps  // shared bundle; nil only in legacy NewSession callers
	agent            *Agent
	store            store.RuntimeStore
	models           *model.ModelRouter
	codec            *protocol.Codec
	cmds             *CommandRegistry
	tools            *tool.ToolManager   // per-session child manager; required (NewSession derives it)
	rootTools        *tool.ToolManager   // parent (agent-level) manager; passed to subagents
	skills           *skill.SkillManager // optional; consulted for per-skill max_turns
	perms            perm.Service        // optional; consulted by tool handlers (skill_files etc.)
	maxToolIters     int                 // 0 → defaultMaxToolIterations
	maxToolItersHard int                 // 0 → 2 × resolved soft cap
	logger           *slog.Logger

	// sessionTools is the static dispatch table. Per-tool init() funcs
	// in tools_subagent.go / tools_plan.go / … register their entries
	// at package-init time; the table is read-only thereafter so
	// dispatch needs no lock.
	sessionTools map[string]sessionToolDescriptor

	// state
	state sync.Map // string → any; for tool handlers to stash arbitrary bits without a dedicated field in Session. Not persisted; lost on restart.

	// Per-session tool snapshot cache. Phase 4.1a stage A step 8
	// moved skill-bindings filtering and per-session caching out
	// of pkg/tool — Manager returns the unfiltered union, Session
	// caches the filtered view here keyed by (toolGen, skillGen,
	// policyGen). See snapshot_cache.go.
	snapMu    sync.Mutex
	snapCache snapshotCache

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

	// childWG tracks this session's direct children (sub-agent
	// goroutines). Each Spawn does Add(1); the deregister callback
	// invoked on child Run exit does Done(). Run's ctx.Done teardown
	// waits on childWG so sequential teardown is "subagents fully
	// exit, then we run our own lifecycle.Release + handleExit". A
	// session never tree-walks descendants — children own their own
	// sub-trees the same way.
	childWG sync.WaitGroup

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

	// plan is the projection of plan_op events for this session
	// (US2). Built once at materialise and updated incrementally
	// by the four plan_* tool handlers. planMu serialises the
	// emit-then-Apply sequence so concurrent tool handlers can't
	// race the in-memory mirror; the persisted events remain the
	// source of truth so a desync (handler crashed mid-update,
	// say) self-heals on the next materialise / restart.
	planMu sync.Mutex
	plan   plan.Plan

	// whiteboard is the in-memory projection of whiteboard_op
	// events (US3). On a host session it carries the canonical
	// message log + NextSeq; on a member session it carries the
	// member's own snapshot of broadcasts received. Built once at
	// materialise and updated by the four whiteboard_* tool
	// handlers (host side) and the member-side internal handler
	// for inbound whiteboard_message Frames. whiteboardMu serialises
	// the seq-allocate + emit + Apply sequence on the host so two
	// concurrent broadcasts can't reuse the same seq.
	whiteboardMu sync.Mutex
	whiteboard   whiteboard.Whiteboard

	// stuck is the in-memory rising-edge state for the three stuck-
	// detection heuristics (phase-4-spec §8.3). Owned by Run; mutated
	// only by handleToolResult / drainPendingInbound on the same
	// goroutine. Restart resets the flags to zero — phase-4 keeps the
	// detection state in-memory by design (spec §8.3 "in-memory only").
	stuck stuckState

	// softWarningDone caches "we already injected the soft-warning
	// nudge for this session". Loaded once at materialise from the
	// session's events (event-source as truth) and flipped at the
	// moment the runtime emits the system_message{kind:"soft_warning"}
	// frame so the loop skips re-emit on every subsequent turn
	// boundary. Restart-safe: a fresh boot reads the flag back from
	// session_events on materialise.
	softWarningDone atomic.Bool

	in     chan protocol.Frame
	out    chan protocol.Frame
	closed atomic.Bool

	// closeReason is the verbatim reason the next SessionClose-driven
	// teardown writes into the persisted session_terminated row. Set
	// by routeInbound just before returning the close sentinel; read
	// by teardown as the event-driven counterpart to context.Cause.
	// Owned by the Run goroutine — no lock needed.
	closeReason string

	// closing transitions to true the moment the session decides
	// (or is told) to terminate, but BEFORE the goroutine has actually
	// torn down. Phase 4.1b-pre stage B introduces this idle gate so a
	// session that has emitted close_requested can keep its Run loop
	// alive (in-flight tool dispatches, subagent_result Submit on
	// parent, etc.) while rejecting any new UserMessage. closed=true
	// is the strictly later transition, set in handleExit after the
	// session_terminated row lands.
	closing atomic.Bool

	// subagentResultSent is set the first time emitSubagentResultToParent
	// fires for this session. requestClose on a subagent fires the
	// emit early so the parent can route SessionClose back; handleExit
	// on the SessionClose-driven exit must NOT re-emit. Cancel-
	// initiated paths (parent_cascade, subagent_cancel via SessionClose)
	// reach handleExit without requestClose having fired locally — so
	// handleExit emits there, and the flag flips at that point too.
	subagentResultSent atomic.Bool
	// done is closed by Run on exit. External callers (Manager.Terminate,
	// Stop) wait on it to know the session goroutine has
	// finished its exit handler — including any session_terminated
	// event append.
	done chan struct{}
}

// terminationCause is the cancel cause attached to a per-session ctx
// when teardown wants to write a `session_terminated` event on exit.
// Phase 4.1b-pre stage B replaces the old s.terminate(cancel-with-cause)
// path with the SessionClose Frame protocol; the only remaining
// producer of *terminationCause is teardown itself, synthesising one
// from s.closeReason so handleExit's existing logic (write reason,
// optional SessionClosed) continues to work.
//
// Graceful shutdown (rootCancel without cause AND no closeReason)
// leaves tc nil so the goroutine writes nothing (FR-028 / FR-029).
type terminationCause struct {
	reason    string
	emitClose bool
	writeCtx  context.Context
}

// sessionCloseSignal is the sentinel error routeInbound returns when
// it observes an inbound *protocol.SessionClose. The Run loop catches
// it via errors.As, copies the reason to s.closeReason, and exits
// the select after running teardown — replacing the historic
// ctx.Done()-driven exit for explicit terminations.
type sessionCloseSignal struct {
	reason string
}

func (e *sessionCloseSignal) Error() string {
	return "session: SessionClose received: " + e.reason
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

// WithMaxToolIterationsHard overrides the per-Turn HARD ceiling on
// model→tool→model loops (phase-4-spec §8.2). Precedence: per-skill
// metadata.hugen.max_turns_hard > this option > 2 × resolved soft cap.
// Tests use this to drive narrow ceilings without provisioning a
// SkillManager.
func WithMaxToolIterationsHard(n int) SessionOption {
	return func(s *Session) {
		if n > 0 {
			s.maxToolItersHard = n
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

// WithPerms attaches a perm.Service the session-scoped tool
// handlers consult (today only `skill_files` for the per-skill
// gate). Sessions opened without WithPerms see s.perms == nil and
// gates that depend on it return ErrPermissionDenied or skip the
// check, depending on the handler.
func WithPerms(p perm.Service) SessionOption {
	return func(s *Session) { s.perms = p }
}

// NewSession constructs a Session bound to its dependencies. The
// passed *tool.ToolManager is the agent-level (root) manager —
// NewSession immediately derives a per-session child via
// tools.NewChild(), registers the session itself as a
// ToolProvider on that child, and stores both views: the child as
// s.tools (consumed by Dispatch / Snapshot) and the root as
// s.rootTools (passed forward when the session spawns sub-agents,
// each of which derives its own child off the same root).
func NewSession(
	id string,
	agent *Agent,
	store store.RuntimeStore,
	models *model.ModelRouter,
	cmds *CommandRegistry,
	codec *protocol.Codec,
	tools *tool.ToolManager,
	logger *slog.Logger,
	opts ...SessionOption,
) *Session {
	if logger == nil {
		logger = slog.Default()
	}
	if tools == nil {
		// Tests that need a tool-less session can pass an empty root
		// (NewToolManager with no providers) — a nil here is a
		// programming error. Panic so the misuse surfaces at
		// construction rather than the first dispatch attempt.
		panic("session: NewSession requires a non-nil *tool.ToolManager")
	}
	s := &Session{
		id:        id,
		agent:     agent,
		store:     store,
		models:    models,
		codec:     codec,
		cmds:      cmds,
		rootTools: tools,
		tools:     tools.NewChild(),
		logger:    logger,
		overrides: make(map[model.Intent]model.ModelSpec),
		in:        make(chan protocol.Frame, 16),
		out:       make(chan protocol.Frame, 32),
		done:      make(chan struct{}),
	}
	for _, o := range opts {
		o(s)
	}
	// Register the session as a ToolProvider on its own child
	// manager — handlers dispatch back to s.callXxx methods. Done
	// after options run so any option that flips a field the
	// provider consumes (perms, skills) sees the final state.
	s.initTools()

	if err := s.tools.AddProvider(s); err != nil {
		// Self-registration failure indicates a name collision in
		// the parent manager (someone already registered "session").
		// That's a programming error in the boot wiring; surface it
		// loudly.
		panic(fmt.Sprintf("session: register self on child tool manager: %v", err))
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
// External callers (Manager.Close, Manager.Suspend, Stop,
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

// Notepad returns the session's notepad handle if the notepad
// extension is registered on the runtime; nil otherwise. The
// handle lives in the session's [extension.SessionState] under the
// well-known key the notepad extension owns.
//
// This accessor is a transitional shim — direct callers should
// migrate to extension/notepad.FromState(s) and read state via
// the [extension.SessionState] surface.
func (s *Session) Notepad() *notepad.Notepad {
	v, ok := s.Value("notepad")
	if !ok {
		return nil
	}
	n, _ := v.(*notepad.Notepad)
	return n
}

// Tools exposes the per-session ToolManager. This is the child
// manager NewSession derived off the root passed at construction:
// per_session providers register on the child; child.Resolve /
// Dispatch / Snapshot walk to the agent-level root for unknown
// providers.
func (s *Session) Tools() *tool.ToolManager { return s.tools }

// RootTools exposes the agent-level (root) ToolManager passed at
// construction. parent.Spawn passes this to the child session's
// constructor so each subagent derives its own per-session child
// off the same root — the per_agent providers (hugr-main,
// duckdb-mcp, …) stay singletons; per_session providers spawn
// once per session.
func (s *Session) RootTools() *tool.ToolManager { return s.rootTools }

// OpenedAt returns the timestamp the session row was first written
// (CreatedAt on SessionRow). Useful for callers that want to echo
// the persisted opened_at without an extra LoadSession.
func (s *Session) OpenedAt() time.Time { return s.openedAt }

// Depth returns the sub-agent depth: 0 for roots, parent.Depth()+1
// for children. Read-only; depth is set at construction and never
// mutated.
func (s *Session) Depth() int { return s.depth }

// Discard cancels the session's per-session ctx without writing
// any terminal event. Used for race-loser disposal of a freshly-
// built session whose registration in Manager.live lost to an
// existing entry — the row is on disk and the SessionOpened frame
// was emitted, but no goroutine is running for this loser instance.
// Don't call after Start; Run owns the cancel after that point.
func (s *Session) Discard() {
	if s.cancel != nil {
		s.cancel(nil)
	}
}

// Materialise lazily reconstructs the session's working window
// from session_events on the first inbound Frame after open or
// resume. Idempotent — second call observes the materialised flag
// and returns nil. Tests that need to bring a freshly-resumed
// session into a model-visible state can call this directly
// instead of routing a frame through Submit.
func (s *Session) Materialise(ctx context.Context) error {
	return s.materialise(ctx)
}

// MarkMaterialised sets the in-memory materialised flag without
// running the event walk. Useful for tests that want to skip
// projection rebuilding for a session whose history doesn't matter
// for the assertion under test.
func (s *Session) MarkMaterialised() { s.materialised.Store(true) }

// SystemPrompt renders the system prompt the session would feed
// into the next turn given its current state (skills, plan,
// whiteboard projection, identity). The result is the same string
// the model sees on its next turn — tests can assert prompt
// composition without driving a full turn.
func (s *Session) SystemPrompt(ctx context.Context) string {
	return s.systemPrompt(ctx)
}

// WhiteboardSnapshot returns a deep copy of the whiteboard
// projection taken under the projection mutex. Tests use this to
// assert host vs. member projections without poking the internal
// mutex/field directly. The returned struct is independent of the
// session's live projection — mutations on it do not flow back.
func (s *Session) WhiteboardSnapshot() whiteboard.Whiteboard {
	s.whiteboardMu.Lock()
	defer s.whiteboardMu.Unlock()
	wb := s.whiteboard
	if len(wb.Messages) > 0 {
		wb.Messages = append([]whiteboard.Message(nil), wb.Messages...)
	}
	return wb
}

// ActiveToolFeed returns the currently-registered ToolFeed for the
// session, or nil if no blocking system tool is in flight. Tests
// poll this to observe the activeToolFeed slot transition without
// reaching into the atomic pointer field.
func (s *Session) ActiveToolFeed() *ToolFeed {
	return s.activeToolFeed.Load()
}

// PlanSnapshot returns a copy of the plan projection taken under
// the projection mutex. Tests use this to assert plan state without
// poking the planMu / plan fields directly.
func (s *Session) PlanSnapshot() plan.Plan {
	s.planMu.Lock()
	defer s.planMu.Unlock()
	return s.plan
}

// MarkClosed forces the session-closed flag on. Tests use this to
// drive tool handlers down the `session_gone` branch without
// running a full teardown. Production code never calls this — Run
// owns s.closed via teardown.
func (s *Session) MarkClosed() { s.closed.Store(true) }

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
	row, summary, perr := store.FrameToEventRow(f, s.agent.ID())
	if perr != nil {
		return fmt.Errorf("session %s: project frame: %w", s.id, perr)
	}
	// Allocate the seq cursor BEFORE AppendEvent so the in-memory
	// Frame can be tagged with its seq atomically with persistence.
	// emit is reachable from multiple goroutines per session — the
	// Run loop (model events, drainPendingInbound, inline handlers)
	// AND tool dispatcher goroutines (Spawn's subagent_started emit,
	// dispatchToolCall's tool_call/tool_result, wait_subagents's
	// consumed-result emit). Concurrent emits are made safe by the
	// store's NextSeq + AppendEvent serialisation (per-session seq
	// uniqueness enforced at the store layer); the channel send onto
	// s.out is itself goroutine-safe. The remaining nuance — that
	// outbox arrival order can interleave between two concurrent
	// emits when their AppendEvent commits race — is acceptable for
	// adapters, which read events back through ListEvents (ordered
	// by seq) for any post-hoc correctness check.
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
//
// Sequential teardown on ctx.Done (cause attached or not):
//
//  1. cancel any in-flight turn and wait for the per-turn goroutines
//     (model streamer + tool dispatcher) to drain. No timeout —
//     bounded turn-runners are the model/tool layer's job; pinning
//     here would mask leaks.
//  2. wait for direct children's goroutines to exit (their ctx is
//     derived from ours, so they already started cancelling). Drain
//     s.in concurrently so a child's last Submit (subagent_result,
//     etc.) doesn't deadlock against a full buffer.
//  3. persist anything we captured in the drain into our event log
//     (no outbox push; outbox is shutting down).
//  4. release per-session resources via lifecycle.Release.
//  5. run handleExit which appends session_terminated and emits the
//     parent-bound subagent_result if applicable.
//  6. close s.out + s.done so external waiters unblock.
//
// All store writes thread the caller's writeCtx (carried on the
// terminationCause) — never context.Background, never a side-band
// timeout. Graceful shutdown (cause==nil) skips steps 3–5.
func (s *Session) Run(ctx context.Context) error {
	defer close(s.done) // signal external waiters BEFORE outbox closes
	defer close(s.out)
	for {
		select {
		case <-ctx.Done():
			s.teardown(ctx)
			return ctx.Err()
		case f, ok := <-s.in:
			if !ok {
				return nil
			}
			if err := s.routeInbound(ctx, f); err != nil {
				if cs, ok := errors.AsType[*sessionCloseSignal](err); ok {
					// Phase 4.1b-pre stage B: event-driven teardown. The
					// SessionClose Frame is the unified close trigger;
					// teardown reads s.closeReason as the reason for the
					// persisted session_terminated row.
					s.closeReason = cs.reason
					s.teardown(ctx)
					return nil
				}
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

// teardown is the ordered shutdown sequence run when the per-session
// ctx fires OR a SessionClose Frame is observed inbound. See Run for
// the prose contract.
//
// Two paths:
//
//   - graceful (ctx.Done with no cause AND s.closeReason==""): step 1
//     (turn cancel + wait) and step 2 (children exit + inbox drain)
//     still run because every running goroutine must release its slot
//     before the binary can exit, but no events are persisted and no
//     Release is called. This matches the phase-4 promise that graceful
//     shutdown writes nothing — orphaned sessions are the restart
//     walker's job on next boot.
//   - explicit (s.closeReason set by routeInbound on SessionClose, OR
//     legacy ctx.Cause(*terminationCause) for sites still using
//     s.terminate): every step runs. Steps 3–5 use tc.writeCtx so a
//     still-honest deadline reaches the store; cancellation of the
//     writeCtx aborts the persist, which is the caller's choice.
func (s *Session) teardown(runCtx context.Context) {
	// 1) Stop the in-flight turn.
	if s.turnCancel != nil {
		s.turnCancel()
	}
	s.turnWG.Wait()

	// Phase 4.1b-pre stage B: explicit close is signalled by an
	// inbound SessionClose Frame whose reason routeInbound stashed in
	// s.closeReason. Synthesise a terminationCause so handleExit's
	// existing reason-handling logic stays unchanged. Graceful
	// shutdown leaves closeReason empty → tc nil → no terminal write.
	var tc *terminationCause
	if s.closeReason != "" {
		tc = &terminationCause{
			reason:    s.closeReason,
			emitClose: emitSessionClosedForReason(s.closeReason),
			writeCtx:  runCtx,
		}
	}

	// Cascade: forward SessionClose to every direct child so their
	// Run loops exit teardown via the same event path. Without this
	// the SessionClose-driven teardown deadlocks waiting on childWG —
	// children's ctx is no longer the cascade signal under D6.
	// Graceful (tc==nil) path skips forwarding: rootCtx cancellation
	// already propagates through every session ctx so children see
	// their own ctx.Done and run a graceful (write-nothing) teardown.
	if tc != nil {
		s.cascadeSessionCloseToChildren(tc.writeCtx, tc.reason)
	}

	// 2) Wait for direct children, draining inbox concurrently so a
	// child's last Submit (subagent_result, etc.) lands on us instead
	// of blocking forever on a full buffer.
	s.drainOnTeardown()

	if tc == nil {
		// Graceful: nothing else to do. handleExit early-returns on a
		// nil cause; replicate that here without the store writes.
		return
	}

	// 3) Persist anything we caught in the drain. emit() short-circuits
	// once s.closed is set, but here closed is still false — we use
	// persistOnly to skip the outbox push (it's about to close anyway).
	for _, f := range s.pendingInbound {
		if err := s.persistOnly(tc.writeCtx, f); err != nil {
			s.logger.Warn("session: persist pending inbound on teardown",
				"session", s.id, "kind", string(f.Kind()), "err", err)
		}
	}
	s.pendingInbound = nil

	// 4) Release per-session resources before we write the terminal
	// event so a future restart-walker reading session_terminated
	// doesn't see a row whose resources are still alive.
	if s.deps != nil && s.deps.Lifecycle != nil {
		if err := s.deps.Lifecycle.Release(tc.writeCtx, s.id); err != nil {
			s.logger.Warn("session: lifecycle release on teardown",
				"session", s.id, "err", err)
		}
	}

	// 5) handleExit appends session_terminated, emits subagent_result
	// to the parent (if any), and (optionally) the SessionClosed
	// outbox frame.
	s.handleExit(runCtx, tc)
}

// cascadeSessionCloseToChildren forwards a SessionClose Frame to
// every direct child so the cascade flows through the event path
// (phase-4.1b-pre stage B / D6 step 22). Each child's Run loop
// receives the Frame, runs teardown with reason="parent_cascade",
// writes its own session_terminated row, and exits — childWG.Done
// fires through the spawn closure regardless of cascade origin.
//
// Idempotent at the child level: a child already in teardown (its
// goroutine has begun closing the Done channel) accepts the Submit
// as a no-op. The Submit's defer-recover handles the brief window
// where s.in is closed but Done has not yet been signalled.
func (s *Session) cascadeSessionCloseToChildren(ctx context.Context, _ string) {
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
		f := protocol.NewSessionClose(c.id, s.agent.Participant(),
			protocol.TerminationCancelCascade)
		c.Submit(ctx, f)
	}
}

// emitSessionClosedForReason maps a SessionClose reason to whether the
// session should also emit a transcript-visible SessionClosed Frame.
// Reasons that originate from a transcript-side action (handleSlashCommand
// /end already emitted SessionClosed; subagent paths surface via
// subagent_result; cascade is internal lifecycle only) suppress the
// extra frame; everything else (Manager.Terminate, hard_ceiling,
// stream_error) emits.
func emitSessionClosedForReason(reason string) bool {
	switch {
	case reason == "":
		return false
	case strings.HasPrefix(reason, "user:"):
		return false
	case reason == protocol.TerminationCancelCascade:
		return false
	case strings.HasPrefix(reason, protocol.TerminationSubagentCancelPrefix):
		return false
	case reason == "subagent_done":
		return false
	case reason == "parent_cascade":
		return false
	default:
		return true
	}
}

// drainOnTeardown blocks until s.childWG fires (every direct child's
// goroutine has exited and run its deregister callback), draining
// s.in into pendingInbound concurrently so a child's last Submit on
// us doesn't deadlock the tree.
//
// After childWG is observed done we drain whatever Frames raced into
// s.in between Submit and childWG.Done — the Submit happens before
// the deregister callback, but Submit returns immediately on a buffer
// hit so the Frame is in s.in well before Done.
func (s *Session) drainOnTeardown() {
	childrenDone := make(chan struct{})
	go func() {
		s.childWG.Wait()
		close(childrenDone)
	}()
	for {
		select {
		case f, ok := <-s.in:
			if !ok {
				// Inbox closed (only happens via s.in close, which
				// nobody does today). Treat as drained.
				return
			}
			s.pendingInbound = append(s.pendingInbound, f)
		case <-childrenDone:
			// Children gone; drain everything that's already in the
			// buffer and exit. Use a non-blocking select so we don't
			// hang waiting for never-arriving frames.
			for {
				select {
				case f, ok := <-s.in:
					if !ok {
						return
					}
					s.pendingInbound = append(s.pendingInbound, f)
				default:
					return
				}
			}
		}
	}
}

// persistOnly appends a Frame to the session's event log without
// pushing it to the outbox. Used by teardown to flush
// pendingInbound after the outbox close path is committed but
// before handleExit writes the terminal row. Mirrors emit's seq
// allocation so seq monotonicity holds across the boundary.
func (s *Session) persistOnly(ctx context.Context, f protocol.Frame) error {
	row, summary, err := store.FrameToEventRow(f, s.agent.ID())
	if err != nil {
		return fmt.Errorf("session %s: project frame on teardown: %w", s.id, err)
	}
	nextSeq, err := s.store.NextSeq(ctx, s.id)
	if err != nil {
		return fmt.Errorf("session %s: next seq on teardown: %w", s.id, err)
	}
	row.Seq = nextSeq
	if setter, ok := f.(protocol.SeqSetter); ok {
		setter.SetSeq(nextSeq)
	}
	if err := s.store.AppendEvent(ctx, row, summary); err != nil {
		return fmt.Errorf("session %s: append frame on teardown: %w", s.id, err)
	}
	return nil
}

// handleExit runs as the final step of teardown when the per-session
// ctx fires with a terminationCause. Two reason cases:
//
//   - explicit (s.terminate(cause) — Manager.Terminate, /end,
//     callSubagentCancel): explicitTerminate=true. Write
//     session_terminated{tc.reason}; optionally emit SessionClosed
//     for adapter back-compat (tc.emitClose=true).
//
//   - cascade (parent's ctx was cancelled with cause; ours derives
//     from parent.ctx so we see the same cause but never set our
//     own explicit flag): explicitTerminate=false. Write
//     session_terminated{cancel_cascade} and suppress SessionClosed
//     — cascade is an internal lifecycle event, not a transcript
//     message.
//
// Graceful shutdown (cause==nil) never reaches here — teardown
// short-circuits before calling handleExit.
//
// All store writes use tc.writeCtx (the caller's ctx threaded
// through terminate). A cancelled writeCtx aborts the persist; that
// is the caller's choice and we honour it.
//
// Phase 4.1b-pre stage B: tc is now passed in by teardown rather than
// recovered from context.Cause. The SessionClose-driven path
// synthesises tc from s.closeReason without cancelling runCtx; the
// legacy s.terminate path still attaches *terminationCause via
// context.WithCancelCause. teardown unifies both into the tc param.
func (s *Session) handleExit(runCtx context.Context, tc *terminationCause) {
	_ = runCtx
	if tc == nil {
		return
	}
	if s.closed.Load() {
		return
	}
	reason := tc.reason
	emitClose := tc.emitClose
	writeCtx := tc.writeCtx
	// Persist session_terminated directly through the store: emit()
	// short-circuits on s.closed (guarding against post-exit writes
	// from racing handlers), but the terminal event IS the close
	// signal — it has to land before we set s.closed=true. After this
	// point any concurrent emit returns ErrSessionClosed cleanly.
	terminal := protocol.NewSessionTerminated(s.id, s.agent.Participant(), protocol.SessionTerminatedPayload{
		Reason: reason,
	})
	if termRow, summary, perr := store.FrameToEventRow(terminal, s.agent.ID()); perr == nil {
		if nextSeq, serr := s.store.NextSeq(writeCtx, s.id); serr == nil {
			termRow.Seq = nextSeq
			if setter, ok := any(terminal).(protocol.SeqSetter); ok {
				setter.SetSeq(nextSeq)
			}
		}
		if err := s.store.AppendEvent(writeCtx, termRow, summary); err != nil {
			s.logger.Warn("session: append session_terminated", "session", s.id, "err", err)
		}
	} else {
		s.logger.Warn("session: project session_terminated", "session", s.id, "err", perr)
	}
	s.closed.Store(true)
	// Surface subagent_result to the parent on every explicit
	// terminate (including cascade). Live delivery via Submit feeds
	// any active wait_subagents through the routing layer; on Submit
	// failure (parent inbox closed because parent itself is shutting
	// down) the result is appended directly to parent's events so a
	// future settleDanglingSubagents pass or wait_subagents cached
	// lookup still sees the terminal row.
	//
	// Symmetric with recover.go's settleDanglingSubagents synthetic
	// emit (US6): settle handles the "child died without graceful
	// exit, parent re-attached at boot" case; this handler covers
	// every other live terminate path (subagent_cancel, /end, parent
	// cascade).
	if s.parent != nil {
		s.emitSubagentResultToParent(writeCtx, reason)
	}
	// SessionClosed is the model-/adapter-visible counterpart;
	// best-effort outbox push is fine since closed=true is now set
	// and emit will recover-safely panic on a closed outbox.
	if emitClose {
		closed := protocol.NewSessionClosed(s.id, s.agent.Participant(), reason)
		if cRow, cSum, perr := store.FrameToEventRow(closed, s.agent.ID()); perr == nil {
			if nextSeq, serr := s.store.NextSeq(writeCtx, s.id); serr == nil {
				cRow.Seq = nextSeq
				if setter, ok := any(closed).(protocol.SeqSetter); ok {
					setter.SetSeq(nextSeq)
				}
			}
			if err := s.store.AppendEvent(writeCtx, cRow, cSum); err != nil {
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

// emitSubagentResultToParent surfaces this session's terminal state
// to its parent at exit time. Live path: Submit to parent's inbox
// so any active wait_subagents tool feed catches it; the routing
// layer either delivers via RouteToolFeed (consumed + persisted by
// wait_subagents) or buffers via RouteBuffered (persisted at the
// next turn boundary's drain).
//
// Offline path: if Submit fails because the parent's inbox has
// already closed (parent terminated first), append the SubagentResult
// directly to parent's events via the store so a future
// drainCachedSubagentResults call or settleDanglingSubagents pass
// still surfaces the terminal row.
//
// Called once from handleExit per session lifetime — no in-handler
// dedup against an existing parent-side subagent_result row, since
// the goroutine reaches handleExit at most once. The settle primitive
// (recover.go) is the deduplication site for any subagent_result row
// the parent might already carry from an earlier path.
func (s *Session) emitSubagentResultToParent(persistCtx context.Context, reason string) {
	parent := s.parent
	if parent == nil {
		return
	}
	if !s.subagentResultSent.CompareAndSwap(false, true) {
		// Phase 4.1b-pre stage B: requestClose may emit the result
		// early so the parent's handleSubagentResult can issue
		// SessionClose back. handleExit must not duplicate the row.
		return
	}
	result := protocol.NewSubagentResult(parent.id, s.id, s.agent.Participant(),
		protocol.SubagentResultPayload{
			SessionID: s.id,
			Reason:    reason,
		})
	if parent.Submit(persistCtx, result) {
		return
	}
	// Parent inbox closed — fall back to direct store append.
	resRow, summary, err := store.FrameToEventRow(result, s.agent.ID())
	if err != nil {
		s.logger.Warn("session: project subagent_result for parent",
			"parent", parent.id, "child", s.id, "err", err)
		return
	}
	if nextSeq, err := s.store.NextSeq(persistCtx, parent.id); err == nil {
		resRow.Seq = nextSeq
	}
	if err := s.store.AppendEvent(persistCtx, resRow, summary); err != nil {
		s.logger.Warn("session: append subagent_result to parent",
			"parent", parent.id, "child", s.id, "err", err)
	}
}

// handleSubagentResult is the parent-side reaction to a child's
// terminal result (phase-4.1b-pre stage B / D6). The child has
// already called requestClose, emitting subagent_result and
// markClosing — its Run loop is idle waiting for the SessionClose
// trigger. Parent issues that trigger here, waits for the child's
// goroutine to exit (instant teardown since the child is already
// idle), and deregisters from s.children before returning.
//
// Subsequent routing of the SubagentResult Frame itself happens in
// routeInbound's RouteToolFeed / RouteBuffered fallthrough so
// wait_subagents (or the next turn boundary's drain) still observes
// the frame.
//
// Idempotent: a second result for the same childID after the first
// has cleaned up the child entry is a no-op (lookup misses).
func (s *Session) handleSubagentResult(ctx context.Context, f *protocol.SubagentResult) {
	childID := f.Payload.SessionID
	if childID == "" {
		childID = f.FromSessionID()
	}
	s.childMu.Lock()
	child, ok := s.children[childID]
	s.childMu.Unlock()
	if !ok {
		return
	}
	closeFrame := protocol.NewSessionClose(child.id, s.agent.Participant(), "subagent_done")
	if !child.Submit(ctx, closeFrame) {
		// Child goroutine already exited (Done channel closed) — its
		// teardown wrote session_terminated; we just need to clean
		// the child entry below.
	}
	select {
	case <-child.Done():
	case <-ctx.Done():
		// Caller cancelled — leave the entry; another routeInbound
		// pass (or graceful shutdown) will mop up.
		return
	}
	s.childMu.Lock()
	if cur, ok := s.children[childID]; ok && cur == child {
		delete(s.children, childID)
	}
	s.childMu.Unlock()
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
// SessionClose Frames trigger event-driven teardown (phase-4.1b-pre
// stage B / D6): the routeInbound contract returns a sentinel
// errSessionCloseSignal carrying the payload reason; the Run loop
// stores it on s.closeReason and runs teardown without cancelling
// runCtx. This replaces the historic ctx.Cause(*terminationCause)
// path for explicit terminations. Graceful shutdown still flows
// through ctx.Done with no cause.
//
// SessionClosed Frames observed inbound (e.g. legacy /end handler
// returning a SessionClosed frame directly) flow through the buffered
// path so adapters still see them in the transcript; the actual
// termination is triggered separately by the SessionClose Frame the
// /end handler also produces.
func (s *Session) routeInbound(ctx context.Context, f protocol.Frame) error {
	switch v := f.(type) {
	case *protocol.SessionClose:
		s.markClosing()
		return &sessionCloseSignal{reason: v.Payload.Reason}
	case *protocol.SubagentResult:
		// Phase 4.1b-pre stage B: parent issues SessionClose back to
		// the child + waits for its goroutine to exit + deregisters
		// from s.children. The frame itself then continues through the
		// normal RouteToolFeed / RouteBuffered path so wait_subagents
		// (or the next turn's drainPendingInbound) sees it.
		s.handleSubagentResult(ctx, v)
	case *protocol.Cancel:
		return s.handleCancel(ctx, v)
	case *protocol.SlashCommand:
		return s.handleSlashCommand(ctx, v)
	case *protocol.UserMessage:
		if s.IsClosing() {
			// Phase 4.1b-pre stage B: the session is on its way out;
			// drop new user input on the floor with a transcript-visible
			// system_marker so the operator sees what happened.
			marker := protocol.NewSystemMarker(s.id, s.agent.Participant(),
				"user_message_rejected_closing", map[string]any{"author": v.Author().ID})
			return s.emit(ctx, marker)
		}
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
		s.cascadeCancelChildren(ctx)
	}
	return s.emit(ctx, f)
}

// cascadeCancelChildren walks the immediate children map and triggers
// terminate on each. Idempotent on already-closed children. A child
// that's mid-spawn (registered but goroutine not yet running its first
// select) sees turnCtx.Done immediately on entry into Run.
//
// ctx is threaded into each child's terminationCause.writeCtx so the
// child's teardown store writes inherit the cancelling caller's
// deadline / cancellation. Children of children inherit again the
// same way when the cascade rolls through their own handleCancel.
func (s *Session) cascadeCancelChildren(ctx context.Context) {
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
		f := protocol.NewSessionClose(c.id, s.agent.Participant(),
			protocol.TerminationCancelCascade)
		c.Submit(ctx, f)
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
		Notepad:     s.Notepad(),
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
	// event-driven teardown via requestClose. For roots the OnCloseRequest
	// hook spawns a goroutine that Submits SessionClose back through
	// Manager.Terminate; for subagents requestClose surfaces
	// subagent_result to the parent so handleSubagentResult issues
	// SessionClose. handleExit will skip the duplicate SessionClosed
	// emit because the reason starts with "user:" — emitSessionClosedForReason
	// suppresses it.
	if sawClose {
		s.requestClose(ctx, "user:"+f.Payload.Name+" "+closeReason)
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

// resolveHardCeiling picks the per-Turn hard ceiling at which the
// session terminates with reason "hard_ceiling" (phase-4-spec §8.2).
// Precedence: loaded skills' max(metadata.hugen.max_turns_hard) →
// 2 × resolved soft cap (the spec's documented default). Sampled
// once at the top of the user turn alongside the soft cap so the
// two stay coherent through the loop.
func (s *Session) resolveHardCeiling(ctx context.Context, softCap int) int {
	if s.skills != nil {
		if b, err := s.skills.Bindings(ctx, s.id); err == nil && b.MaxTurnsHard > 0 {
			return b.MaxTurnsHard
		}
	}
	if s.maxToolItersHard > 0 {
		return s.maxToolItersHard
	}
	if softCap > 0 {
		return softCap * 2
	}
	return defaultMaxToolIterations * 2
}

// stuckDetectionEnabled reports whether the heuristic stuck-detection
// nudges are active for this session. Returns false only when a loaded
// skill explicitly disables them (Bindings.StuckDetectionDisabled).
// Sessions without a SkillManager keep the conservative default ON.
func (s *Session) stuckDetectionEnabled(ctx context.Context) bool {
	if s.skills == nil {
		return true
	}
	b, err := s.skills.Bindings(ctx, s.id)
	if err != nil {
		return true
	}
	return !b.StuckDetectionDisabled
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
//  1. Active plan block (when set; renders body + current-step
//     pointer at the top so it survives history truncation —
//     phase-4 spec §6.5 + contracts/tools-plan.md "Prompt-rendering
//     contract").
//  2. Agent constitution (universal rules).
//  3. Body of every skill currently loaded into the session
//     (concrete tool-usage instructions for the active toolset).
//  4. Catalogue of every skill the agent can reach — both loaded
//     and unloaded — so the model picks the right one and calls
//     skill_load without a separate discovery tool round-trip.
//     Loaded skills are tagged so the model doesn't reload them.
func (s *Session) systemPrompt(ctx context.Context) string {
	var parts []string
	if block := s.renderPlanBlock(); block != "" {
		parts = append(parts, block)
	}
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

// renderPlanBlock returns the system-prompt-friendly rendering of
// this session's plan projection — empty when no plan is active.
// Holding planMu briefly ensures the read sees a consistent
// snapshot even if a tool handler is mid-Apply on another goroutine.
func (s *Session) renderPlanBlock() string {
	s.planMu.Lock()
	defer s.planMu.Unlock()
	return plan.Render(s.plan)
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
	snap, err := s.fetchSnapshot(ctx)
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
// message and a flag distinguishing success from "an error frame
// just landed instead". The flag drives the stuck-detection
// no_progress detector via toolResultEvent; "" + errored=true is
// the canonical dispatch-failure shape.
//
// Two contexts:
//   - dispatchCtx (turnCtx): threaded into permission resolve and
//     tool.Dispatch so /cancel cleanly aborts long-running tools.
//   - emitCtx (runCtx): used for s.emit so transcript frames keep
//     landing even if the user is mid-cancellation; emit's own ctx
//     is the session's run ctx, never cancelled until process shutdown.
func (s *Session) dispatchToolCall(turnCtx, emitCtx context.Context, tc model.ChunkToolCall) (string, bool) {
	if s.tools == nil {
		s.emitToolError(emitCtx, tc.ID, tc.Name, protocol.ToolErrorNotFound,
			"tool dispatch not configured for this session", "")
		return "", true
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
	snap, snapErr := s.fetchSnapshot(dispatchCtx)
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
		return "", true
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
			return "", true
		}
		s.emitToolError(emitCtx, tc.ID, tc.Name, "io", err.Error(), "")
		return "", true
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
		return "", true
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
	return string(result), false
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

// IsClosed reports whether the session has been closed.
func (s *Session) IsClosed() bool { return s.closed.Load() }

// IsClosing reports whether the session has begun teardown (a
// SessionClose Frame has been requested, either by the session
// itself via requestClose or by an external producer about to
// Submit one). closing transitions to true strictly before
// closed; once true, new UserMessage frames are rejected (the
// session is on its way out).
func (s *Session) IsClosing() bool { return s.closing.Load() }

// markClosing flips the closing flag. Used by requestClose and by
// teardown after a SessionClose Frame is observed inbound. Idempotent.
func (s *Session) markClosing() { s.closing.Store(true) }

// requestClose triggers session teardown through the event-driven
// SessionClose protocol (phase-4.1b-pre §6 Stage B / D6). It emits a
// close_requested system_marker for observability, marks the session
// closing (so subsequent UserMessages are refused), and dispatches
// the close trigger to whoever owns the session's termination:
//
//   - root sessions: deps.OnCloseRequest fires (Manager wiring spawns
//     a goroutine that Submits SessionClose back to the root via
//     Manager.Terminate). The hook never blocks the caller.
//   - subagent sessions: emit subagent_result to the parent's inbox
//     so the parent's handleSubagentResult issues the SessionClose
//     trigger back. The Run loop then sits idle until the trigger
//     arrives.
//
// Idempotent: a second call after closing=true is a no-op (Store is
// already set; OnCloseRequest fires once; subagent_result is gated
// by subagentResultSent).
//
// ctx is the caller's request-scoped context (typically the Run
// loop's runCtx or a turnCtx). Implementations of OnCloseRequest
// decide whether to inherit or detach (context.WithoutCancel) from
// its cancel chain — the hook in pkg/runtime detaches so Manager
// .Terminate survives the Run loop's own teardown.
func (s *Session) requestClose(ctx context.Context, reason string) {
	if s == nil {
		return
	}
	if !s.closing.CompareAndSwap(false, true) {
		return
	}
	marker := protocol.NewSystemMarker(s.id, s.agent.Participant(),
		"close_requested", map[string]any{"reason": reason})
	if err := s.emit(ctx, marker); err != nil && !errors.Is(err, ErrSessionClosed) {
		s.logger.Warn("session: emit close_requested marker",
			"session", s.id, "reason", reason, "err", err)
	}
	if s.parent != nil {
		// Subagent path: surface the terminal result to the parent so
		// its handleSubagentResult can issue SessionClose back. The
		// Run loop stays alive until the SessionClose Frame arrives.
		s.emitSubagentResultToParent(ctx, reason)
		return
	}
	// Root path: hand the close request to whoever owns this tree's
	// termination (Manager). Optional — tests with no Manager wiring
	// must drive teardown via the SessionClose Frame directly.
	if s.deps != nil && s.deps.OnCloseRequest != nil {
		s.deps.OnCloseRequest(ctx, s.id, reason)
	}
}

// LastActive returns time.Now (placeholder; Phase 4 fills this in
// from updated_at if needed).
func (s *Session) LastActive() time.Time { return time.Now().UTC() }
