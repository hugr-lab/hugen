package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// SessionSummary is a lightweight projection of a session row used
// by adapters to list sessions.
type SessionSummary struct {
	ID        string
	Status    string
	OpenedAt  time.Time
	UpdatedAt time.Time
	Metadata  map[string]any
}

// OpenRequest carries the parameters for Manager.Open.
//
// Phase-4 fields:
//
//   - ParentSessionID, SpawnedFromEventID — set by Session.Spawn for
//     sub-agent sessions via newSession(parent, ...); left zero for
//     root sessions opened by adapters. (Depth/SessionType are no
//     longer carried in OpenRequest — newSession derives them from the
//     parent argument.)
type OpenRequest struct {
	OwnerID      string
	Participants []protocol.ParticipantInfo
	// Metadata is persisted verbatim on the session row. Adapters
	// validate size/shape before passing it through; the manager
	// stores it as-is. For sub-agents the manager also writes
	// metadata["depth"] (set to parent.depth+1, immutable) here.
	Metadata map[string]any

	ParentSessionID    string
	SpawnedFromEventID string
}

// SpawnSpec is the input to Session.Spawn. Carries the model-supplied
// fields from session:spawn_subagent (skill, role, task, inputs) plus
// the parent's spawn-event id used for diagnostics.
type SpawnSpec struct {
	Skill   string
	Role    string
	Task    string
	Inputs  any
	EventID string
	// Metadata is merged into the child session row's metadata map
	// after the manager fills in metadata["depth"] / metadata["spawn_role"]
	// / metadata["spawn_skill"]. Caller-supplied keys win on collision.
	Metadata map[string]any
	// ParentWhiteboardActive captures the host's whiteboard projection
	// state at spawn time (FR-035 conditional autoload). Set by
	// callSpawnSubagent (phase-3 commit 10) when the parent's
	// whiteboard projection has Active=true. Phase-4 commit 4 only
	// plumbs the field through to OpenRequest / SubagentStarted so the
	// child's session_started event captures it.
	ParentWhiteboardActive bool
}

// Manager owns the live *Session map and brokers
// open/resume/close. Each Session runs in its own goroutine.
//
// Sessions outlive the adapter goroutines that opened them: if an
// adapter exits cleanly, the session goroutine keeps running until
// either /end fires or the runtime is shut down explicitly. This
// is what makes the long-lived-session promise honest — adapter
// crash != session loss.

type Manager struct {
	store    RuntimeStore
	agent    *Agent
	models   *model.ModelRouter
	commands *CommandRegistry
	codec    *protocol.Codec
	logger   *slog.Logger

	sessionOpts []SessionOption
	lifecycle   Lifecycle

	// deps mirrors the per-session dependency bundle passed by
	// reference to every Session in this Manager's tree (root +
	// subagents). Populated by NewManager from the same arguments
	// that fill the explicit fields above; both views point at the
	// same wg / rootCtx / RuntimeStore so existing manager.go code
	// can continue to read m.store / m.agent / ... without a churn,
	// while newSession / newSessionRestore can take m.deps
	// monolithically.
	deps *sessionDeps

	rootCtx    context.Context
	rootCancel context.CancelFunc

	mu sync.RWMutex
	// live maps root-session id → *Session. Sub-agents are owned by
	// their parent's `children` map and never appear here. Manager
	// is therefore the registry / router for the *forest of trees*;
	// per-tree subagent lookup is parent.FindDescendant.
	live map[string]*Session
	// wg tracks every spawned session goroutine. ShutdownAll uses
	// it to wait for every goroutine to finish exiting BEFORE the
	// local DuckDB engine closes — without this guarantee an
	// in-flight AppendEvent races the engine teardown.
	wg sync.WaitGroup
}

// terminationCause is the cancel cause attached to a per-session ctx
// when Manager.Terminate (or the /end slash command) wants the session
// goroutine to append a `session_terminated` event on exit. Graceful
// process shutdown (rootCancel) uses no cause, so the goroutine
// distinguishes the two paths via context.Cause(ctx) and writes
// nothing on graceful shutdown.
//
// emitClose controls whether the goroutine's exit handler emits a
// SessionClosed Frame to the outbox in addition to appending the
// session_terminated event:
//
//   - emitClose=true  → the caller (Manager.Terminate) hasn't already
//     surfaced a SessionClosed; the goroutine emits one for adapter
//     back-compat.
//   - emitClose=false → the caller already emitted SessionClosed (the
//     /end slash command path); the goroutine writes the event but
//     suppresses a duplicate frame.
type terminationCause struct {
	reason    string
	emitClose bool
}

func (c *terminationCause) Error() string { return "session terminated: " + c.reason }

// ManagerOption configures a Manager at construction.
type ManagerOption func(*Manager)

// WithLifecycle attaches a Lifecycle to the manager. The Lifecycle
// owns per-session resource acquisition and release — typically a
// *Resources constructed by cmd/hugen at boot. A nil Lifecycle
// means the manager opens sessions without per-session resources
// (used by tests that don't wire a workspace or tool stack).
func WithLifecycle(l Lifecycle) ManagerOption {
	return func(m *Manager) { m.lifecycle = l }
}

// WithSessionOptions threads SessionOption values through every
// spawned Session — typically used by cmd/hugen to attach the
// shared *tool.ToolManager via WithTools.
func WithSessionOptions(opts ...SessionOption) ManagerOption {
	return func(m *Manager) {
		m.sessionOpts = append(m.sessionOpts, opts...)
	}
}

// NewManager constructs the manager. All required deps are
// passed in (constitution principle II). The manager owns a root
// context (separate from any adapter's errgroup context) that scopes
// every session goroutine; Shutdown cancels it.
func NewManager(
	store RuntimeStore,
	agent *Agent,
	models *model.ModelRouter,
	commands *CommandRegistry,
	codec *protocol.Codec,
	logger *slog.Logger,
	opts ...ManagerOption,
) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	rootCtx, rootCancel := context.WithCancel(context.Background())
	m := &Manager{
		store:      store,
		agent:      agent,
		models:     models,
		commands:   commands,
		codec:      codec,
		logger:     logger,
		rootCtx:    rootCtx,
		rootCancel: rootCancel,
		live:       make(map[string]*Session),
	}
	for _, o := range opts {
		o(m)
	}
	// Build the shared sessionDeps view AFTER the options ran, so
	// Lifecycle and SessionOption updates picked up by m.lifecycle /
	// m.sessionOpts are reflected in the bundle that newSession /
	// newSessionRestore see.
	m.deps = &sessionDeps{
		store:     m.store,
		agent:     m.agent,
		models:    m.models,
		commands:  m.commands,
		codec:     m.codec,
		logger:    m.logger,
		lifecycle: m.lifecycle,
		opts:      m.sessionOpts,
		rootCtx:   m.rootCtx,
		wg:        &m.wg,
		maxDepth:  defaultMaxDepth,
	}
	return m
}

// defaultMaxDepth is the phase-4 fallback for sessionDeps.maxDepth
// until commit 9 wires cfg.Subagents().DefaultMaxDepth. Matches
// `phase-4-spec.md §5.7` Layer 2 default.
const defaultMaxDepth = 5

// Open creates a fresh root session via newSession, registers it in
// m.live, and starts its goroutine. Returns the session and the row's
// CreatedAt timestamp so callers can echo the persisted opened_at
// without an extra LoadSession.
//
// Phase 4: only roots reach this path. Sub-agents go through
// Manager.Spawn → newSession(ctx, parent, ...) which bypasses Open.
func (m *Manager) Open(ctx context.Context, req OpenRequest) (*Session, time.Time, error) {
	s, err := newSession(ctx, nil, m.deps, req)
	if err != nil {
		return nil, time.Time{}, err
	}
	m.mu.Lock()
	if existing, ok := m.live[s.id]; ok {
		// Race is theoretical for roots (id is random); keep the
		// branch defensive so a duplicate id can't leak goroutines.
		m.mu.Unlock()
		s.cancel(nil)
		return existing, existing.openedAt, nil
	}
	m.live[s.id] = s
	m.mu.Unlock()
	s.start(m.deregisterFn(s.id, s))
	return s, s.openedAt, nil
}

// Resume reattaches to an existing root session row via
// newSessionRestore. Materialisation is deferred to the first inbound
// Frame after resume.
//
// Concurrent calls on the same id are made safe by a post-construction
// double-check on m.live; the loser cancels its freshly-built ctx and
// returns the winner's *Session. (The loser may already have appended
// a session_resumed marker — same hazard as the pre-pivot code path,
// rare in practice.)
func (m *Manager) Resume(ctx context.Context, id string) (*Session, error) {
	m.mu.RLock()
	if existing, ok := m.live[id]; ok {
		m.mu.RUnlock()
		return existing, nil
	}
	m.mu.RUnlock()

	s, err := newSessionRestore(ctx, id, nil, m.deps)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	if existing, ok := m.live[id]; ok {
		m.mu.Unlock()
		s.cancel(nil)
		return existing, nil
	}
	m.live[id] = s
	m.mu.Unlock()
	s.start(m.deregisterFn(id, s))
	return s, nil
}

// Deliver pushes a Frame onto the addressed root session's inbox.
// Pivot 4 narrows m.live to roots only — Deliver is therefore a
// root-only entry point. Cross-tree delivery to a sub-agent goes
// through its parent (parent forwards via Submit), keeping the
// "Manager only routes to roots" invariant clean.
//
// Recover-safe wrapper over Session.Submit so cross-session frame
// delivery has a single audit-friendly entry point and so panics from
// a goroutine racing its own exit don't propagate.
func (m *Manager) Deliver(ctx context.Context, to string, f protocol.Frame) error {
	m.mu.RLock()
	s, ok := m.live[to]
	m.mu.RUnlock()
	if !ok {
		return ErrSessionNotFound
	}
	if !s.Submit(ctx, f) {
		return ErrSessionGone
	}
	return nil
}

// ErrSessionGone is returned by Deliver when the addressed session's
// goroutine has exited (its inbox is closed) — the frame can never
// be delivered.
var ErrSessionGone = errors.New("manager: session goroutine exited")

// Terminate is the unified session-termination path for phase 4 and
// onward. It cancels the target session's ctx with a terminationCause
// carrying the caller-supplied reason and waits for the goroutine to
// append its session_terminated event and exit.
//
// Mechanism:
//
//   - Live session: cancel per-session ctx via CancelCauseFunc;
//     goroutine's ctx.Done handler reads context.Cause(ctx) →
//     appends session_terminated{reason} → emits SessionClosed Frame
//     for adapter back-compat → exits. Then Manager waits on s.Done()
//     and runs lifecycle.Release.
//
//   - Not live (process restart edge case, /end during shutdown,
//     subagent already terminal): append a session_terminated event
//     directly via the store. The sessions row is NOT updated —
//     liveness is event-derived (FR-027).
//
// Idempotent: calling Terminate on a session that already has a
// session_terminated event is a no-op (skipped silently).
func (m *Manager) Terminate(ctx context.Context, id, reason string) error {
	if reason == "" {
		reason = "terminated"
	}
	m.mu.RLock()
	s, live := m.live[id]
	m.mu.RUnlock()
	if live {
		if !s.closed.Load() {
			s.terminate(&terminationCause{reason: reason, emitClose: true})
		}
		select {
		case <-s.Done():
		case <-ctx.Done():
			return ctx.Err()
		}
		if m.lifecycle != nil {
			if err := m.lifecycle.Release(ctx, id); err != nil {
				m.logger.Warn("manager: terminate session lifecycle", "session", id, "err", err)
			}
		}
		return nil
	}
	// Not-live path: append the terminal event ourselves so the
	// session is unambiguously terminated on next boot.
	if existing, err := m.store.ListEvents(ctx, id, ListEventsOpts{Limit: 1000}); err == nil {
		for _, ev := range existing {
			if ev.EventType == string(protocol.KindSessionTerminated) {
				return nil // already terminal
			}
		}
	}
	terminal := protocol.NewSessionTerminated(id, m.agent.Participant(), protocol.SessionTerminatedPayload{
		Reason: reason,
	})
	row, summary, err := FrameToEventRow(terminal, m.agent.ID())
	if err != nil {
		return fmt.Errorf("manager: terminate (offline) project frame: %w", err)
	}
	if err := m.store.AppendEvent(ctx, row, summary); err != nil {
		return fmt.Errorf("manager: terminate (offline) append: %w", err)
	}
	if m.lifecycle != nil {
		if err := m.lifecycle.Release(ctx, id); err != nil {
			m.logger.Warn("manager: terminate (offline) lifecycle", "session", id, "err", err)
		}
	}
	return nil
}

// ListSessions returns lightweight summaries of every session row
// for this agent. Renamed from Manager.List in phase-4 step 6 to free
// the unqualified List slot for the tool.ToolProvider interface
// implementation in pkg/session/manager_tool_provider.go.
func (m *Manager) ListSessions(ctx context.Context, status string) ([]SessionSummary, error) {
	rows, err := m.store.ListSessions(ctx, m.agent.ID(), status)
	if err != nil {
		return nil, err
	}
	out := make([]SessionSummary, 0, len(rows))
	for _, r := range rows {
		out = append(out, SessionSummary{
			ID:        r.ID,
			Status:    r.Status,
			OpenedAt:  r.CreatedAt,
			UpdatedAt: r.UpdatedAt,
			Metadata:  r.Metadata,
		})
	}
	return out, nil
}

// Get returns a live *Session by id (already-running). Used by the
// supervisor goroutine to route inbound frames.
func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.live[id]
	return s, ok
}

// ShutdownAll cancels the root context (which propagates to every
// per-session ctx) and waits for every session goroutine to exit.
//
// **Phase-4 invariant**: graceful shutdown writes nothing — no
// `session_terminated` events are appended. Sessions whose goroutines
// died without a terminal event are exactly the "needs-restart-
// decision" set on the next boot:
//
//   - root sessions resume on next user input (standard phase-3 path);
//   - sub-agent sessions are processed by the restart BFS walker
//     (phase-4 commit 14) which appends
//     `session_terminated{reason:"restart_died"}` to each and delivers
//     a synthetic subagent_result Frame to its parent's inbox.
//
// Idempotent and safe to call multiple times.
func (m *Manager) ShutdownAll(ctx context.Context) {
	m.rootCancel()
	m.wg.Wait()
}

// deregisterFn returns the onExit callback used by root sessions
// (Manager.Open / Manager.Resume) to remove themselves from m.live
// when their goroutine exits. The cur == s identity check guards
// against a re-Open race that registered a fresh session under the
// same id between this session's Run() return and the deregister
// callback firing.
func (m *Manager) deregisterFn(id string, s *Session) func() {
	return func() {
		m.mu.Lock()
		if cur, ok := m.live[id]; ok && cur == s {
			delete(m.live, id)
		}
		m.mu.Unlock()
	}
}


// SessionsLive returns the IDs of currently live sessions.
func (m *Manager) SessionsLive() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.live))
	for id := range m.live {
		out = append(out, id)
	}
	return out
}

func newSessionID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("ses-%d", time.Now().UnixNano())
	}
	return "ses-" + hex.EncodeToString(b[:])
}
