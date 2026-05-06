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
	deps *Deps

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
	// Build the shared Deps view AFTER the options ran, so
	// Lifecycle and SessionOption updates picked up by m.lifecycle /
	// m.sessionOpts are reflected in the bundle that newSession /
	// newSessionRestore see.
	m.deps = &Deps{
		Store:     m.store,
		Agent:     m.agent,
		Models:    m.models,
		Commands:  m.commands,
		Codec:     m.codec,
		Logger:    m.logger,
		Lifecycle: m.lifecycle,
		Opts:      m.sessionOpts,
		RootCtx:   m.rootCtx,
		WG:        &m.wg,
		MaxDepth:  DefaultMaxDepth,
	}
	// Phase 4.1b-pre stage B / D6: a root session calling
	// requestClose hands the close request to Manager via this hook.
	// We spawn a goroutine because Terminate Submits SessionClose +
	// blocks on the session's Done channel — running it inline would
	// deadlock the requesting Run loop. context.WithoutCancel preserves
	// tracing/identity values from the caller's ctx while detaching
	// from its cancel chain so Terminate survives the Run loop's own
	// teardown that follows.
	m.deps.OnCloseRequest = func(ctx context.Context, id, reason string) {
		go func() { _ = m.Terminate(context.WithoutCancel(ctx), id, reason) }()
	}
	return m
}

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
	// Phase 4.1b-pre stage B: roots are removed from m.live by
	// Manager.Terminate after the session's Done channel closes.
	// Graceful shutdown leaves stale entries until the next process
	// boot — m.live is in-memory only and the binary is exiting.
	s.Start(ctx)
	return s, s.openedAt, nil
}

// Resume reattaches to an existing root session row via
// newSessionRestore. Materialisation is deferred to the first inbound
// Frame after resume.
//
// Resume is **root-only**: passing a sub-agent id surfaces
// ErrNotRootSession instead of silently registering the sub-agent in
// m.live and breaking the "m.live is roots only" invariant
// (phase-4-tree-ctx-routing ADR D4). Sub-agents are owned by their
// parent's children map; cross-tree access goes through the parent.
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

	row, err := m.store.LoadSession(ctx, id)
	if err != nil {
		return nil, err
	}
	if row.SessionType != "" && row.SessionType != "root" {
		return nil, fmt.Errorf("manager: cannot resume %s session %q: %w",
			row.SessionType, id, ErrNotRootSession)
	}

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
	s.Start(ctx)
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

// ErrNotRootSession is returned by Manager.Resume when the requested
// id maps to a non-root session row. Manager only tracks roots in
// m.live (phase-4-tree-ctx-routing ADR D4); sub-agents are reachable
// only through their parent.
var ErrNotRootSession = errors.New("manager: not a root session")

// BroadcastSystemMarker pushes a system_marker Frame into every live
// root session's inbox. Used by callers that need to surface a
// runtime-wide event across every active conversation — currently
// the MCP reconnector, which fires `mcp_recovered` so the model on
// each root sees the recovery in its transcript and can retry tools
// that previously surfaced as `provider_removed`.
//
// Best-effort per session: a Submit failure (closed inbox, full
// buffer, ctx cancellation) is logged at Debug and the broadcast
// continues to the next session — one stuck session must not block
// the rest of the rooster.
func (m *Manager) BroadcastSystemMarker(ctx context.Context, subject string, meta map[string]any) {
	m.mu.RLock()
	targets := make([]*Session, 0, len(m.live))
	for _, s := range m.live {
		targets = append(targets, s)
	}
	m.mu.RUnlock()
	for _, s := range targets {
		marker := protocol.NewSystemMarker(s.id, m.agent.Participant(), subject, meta)
		if !s.Submit(ctx, marker) {
			m.logger.Debug("manager: broadcast system_marker dropped",
				"session", s.id, "subject", subject)
		}
	}
}

// Terminate Submits a SessionClose Frame to the addressed *root*
// session and waits for the goroutine to run its sequential teardown
// (turn cancel → children wait → persist pending inbound →
// lifecycle.Release → session_terminated + SessionClosed → close).
// The session's own goroutine owns the terminal write — Manager just
// triggers and waits, then removes the entry from m.live.
//
// Manager only knows roots: m.live[id] is the lookup. Sub-agents are
// owned by their parent's children map; sub-agent termination goes
// through callSubagentCancel + handleSubagentResult, never Manager.
// Calling Terminate with a sub-agent id surfaces ErrSessionNotFound
// — by design, not a bug.
//
// Idempotent: a second call after the goroutine has exited returns
// ErrSessionNotFound (the first call's deregister already removed
// the entry). The Run loop dedups concurrent SessionClose Frames via
// the closing flag — only the first reason wins.
func (m *Manager) Terminate(ctx context.Context, id, reason string) error {
	if reason == "" {
		reason = "terminated"
	}
	m.mu.RLock()
	s, ok := m.live[id]
	m.mu.RUnlock()
	if !ok {
		return ErrSessionNotFound
	}
	closeFrame := protocol.NewSessionClose(s.id, m.agent.Participant(), reason)
	s.Submit(ctx, closeFrame)
	select {
	case <-s.Done():
	case <-ctx.Done():
		return ctx.Err()
	}
	m.mu.Lock()
	if cur, exists := m.live[id]; exists && cur == s {
		delete(m.live, id)
	}
	m.mu.Unlock()
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

// Start runs the Manager-side per-boot tasks. Today: RestoreActive
// (settles dangling subagents + eagerly restores non-terminal roots).
// Reserves the slot for future scheduler-style background work
// (sweep, cron, …) so callers have a single Start/Stop pair to track.
//
// Idempotent: a second call repeats RestoreActive whose own BFS
// walker short-circuits when there is nothing left to settle.
//
// Phase 4.1b-pre stage B (O4) introduces this as the canonical
// public boot entry; cmd/hugen and the upcoming 4.1b harness call
// Start instead of poking RestoreActive directly.
func (m *Manager) Start(ctx context.Context) error {
	return m.RestoreActive(ctx)
}

// Stop cancels the root context (which propagates to every
// per-session ctx) and waits for every session goroutine to exit.
// Renamed from ShutdownAll in phase-4.1b-pre stage B (O4) — the
// single public exit point paired with Start.
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
func (m *Manager) Stop(ctx context.Context) {
	_ = ctx
	m.rootCancel()
	m.wg.Wait()
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
