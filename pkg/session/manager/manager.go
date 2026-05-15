// Package manager owns the multi-session supervisor that opens,
// resumes, and terminates root sessions plus the boot-time
// recovery walker that settles dangling sub-agents after a process
// restart. The Session itself (turn loop, frame routing, tool
// dispatch, persistence projection) lives in pkg/session.
package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/prompts"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session"
	"github.com/hugr-lab/hugen/pkg/session/store"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// Manager owns the live *Session map and brokers
// open/resume/close. Each Session runs in its own goroutine.
//
// Sessions outlive the adapter goroutines that opened them: if an
// adapter exits cleanly, the session goroutine keeps running until
// either /end fires or the runtime is shut down explicitly. This
// is what makes the long-lived-session promise honest — adapter
// crash != session loss.

type Manager struct {
	store    store.RuntimeStore
	agent    *session.Agent
	models   *model.ModelRouter
	commands *session.CommandRegistry
	codec    *protocol.Codec
	tools    *tool.ToolManager
	logger   *slog.Logger

	sessionOpts []session.SessionOption
	extensions  []extension.Extension

	// defaultMissionSkill is the fallback skill name for
	// session:spawn_mission when the root model omits the `skill`
	// argument. Set via WithDefaultMissionSkill; propagates into
	// Deps so spawn_mission can resolve it. Phase 4.2.2 §6.
	defaultMissionSkill string

	// tierIntents maps session tier → model-router intent.
	// Set via WithTierIntents; propagates into Deps so per-tier
	// model routing applies at spawn time. Phase 4.2.2 §11.
	tierIntents map[string]string

	// prompts is the agent-level template renderer shared by
	// every session in the tree. Set via WithPrompts; propagates
	// into Deps so extension Advertisers and session-internal
	// interrupt-text generators can render bundled templates.
	// Phase 5.1 §α.2.
	prompts *prompts.Renderer

	// maxAsyncMissionsPerRoot caps in-flight children at the root
	// of every spawn chain (counted at the time of an async spawn).
	// 0 disables enforcement; runtime defaults to 5 unless
	// WithMaxAsyncMissionsPerRoot overrides. Phase 5.1 § 4.5.
	maxAsyncMissionsPerRoot int

	// defaultInquireTimeoutMs is the per-call session:inquire
	// deadline used when the model omits timeout_ms; also the
	// upper-bound clamp for caller-supplied timeouts. 0 leaves the
	// pkg/session fallback (1 hour) in place. Phase 5.1 § 2.7.
	defaultInquireTimeoutMs int

	// tierDefaults is the per-tier turn-loop budget defaults map
	// threaded into Deps for the phase-5.2 δ resolution chain. Set
	// via WithTierDefaults. nil leaves the runtime constants
	// (defaultMaxToolIterations / × 2) in place at every tier.
	tierDefaults map[string]session.TierTurnDefaults

	// maxParkedChildrenPerRoot caps simultaneously-parked children
	// across a root's subtree. 0 disables enforcement. Phase 5.2 ε.
	maxParkedChildrenPerRoot int

	// parkedIdleTimeout is the per-parked-child idle deadline.
	// 0 disables the timer. Phase 5.2 ε.
	parkedIdleTimeout time.Duration

	// deps mirrors the per-session dependency bundle passed by
	// reference to every Session in this Manager's tree (root +
	// subagents). Populated by NewManager from the same arguments
	// that fill the explicit fields above; both views point at the
	// same wg / rootCtx / RuntimeStore so existing manager.go code
	// can continue to read m.store / m.agent / ... without a churn,
	// while newSession / newSessionRestore can take m.deps
	// monolithically.
	deps *session.Deps

	rootCtx    context.Context
	rootCancel context.CancelFunc

	mu sync.RWMutex
	// live maps root-session id → *Session. Sub-agents are owned by
	// their parent's `children` map and never appear here. Manager
	// is therefore the registry / router for the *forest of trees*;
	// per-tree subagent lookup is parent.FindDescendant.
	live map[string]*session.Session
	// wg tracks every spawned session goroutine. Stop uses
	// it to wait for every goroutine to finish exiting BEFORE the
	// local DuckDB engine closes — without this guarantee an
	// in-flight AppendEvent races the engine teardown.
	wg sync.WaitGroup
}

// defaultMaxAsyncMissionsPerRoot is the phase-5.1 § 4.5 default
// when WithMaxAsyncMissionsPerRoot is omitted by the runtime.
const defaultMaxAsyncMissionsPerRoot = 5

// Phase 5.2 ε defaults — applied when the runtime omits the
// corresponding option. Matches pkg/config StaticService numbers
// so a manager built without an operator config (tests, no-skill
// deployments) still gets the runtime-hygiene behaviour.
const (
	defaultMaxParkedChildrenPerRoot = 3
	defaultParkedIdleTimeout        = 10 * time.Minute
)

// ManagerOption configures a Manager at construction.
type ManagerOption func(*Manager)

// WithSessionOptions threads SessionOption values through every
// spawned Session — typically used by cmd/hugen to attach the
// shared *tool.ToolManager via WithTools.
func WithSessionOptions(opts ...session.SessionOption) ManagerOption {
	return func(m *Manager) {
		m.sessionOpts = append(m.sessionOpts, opts...)
	}
}

// WithExtensions registers session extensions (notepad, plan,
// whiteboard, skills, future plugins) on this Manager. Each
// spawned Session iterates the list and dispatches each extension
// to the capability hooks it implements (StateInitializer at open,
// Recovery at materialise, Closer at teardown, …). Order is
// preserved — later extensions read state earlier ones may have
// stashed; Closers run in reverse.
func WithExtensions(exts ...extension.Extension) ManagerOption {
	return func(m *Manager) {
		m.extensions = append(m.extensions, exts...)
	}
}

// WithDefaultMissionSkill sets the fallback skill name for
// session:spawn_mission when the root model omits the `skill`
// argument. Empty (default) means no fallback — spawn_mission
// requires the model to specify a skill explicitly. Phase 4.2.2
// §6.
func WithDefaultMissionSkill(name string) ManagerOption {
	return func(m *Manager) {
		m.defaultMissionSkill = name
	}
}

// WithPrompts installs the agent-level template renderer. The
// renderer is shared across every session in the tree and is
// surfaced both as session.Deps.Prompts (used by interrupt-text
// generators inside pkg/session) and via state.Prompts() through
// extension.SessionState. Phase 5.1 §α.2.
func WithPrompts(r *prompts.Renderer) ManagerOption {
	return func(m *Manager) {
		m.prompts = r
	}
}

// WithMaxAsyncMissionsPerRoot sets the per-root concurrency cap
// for spawn_mission(wait="async"). 0 disables enforcement; the
// manager defaults to 5 when this option is omitted. Phase 5.1
// § 4.5.
func WithMaxAsyncMissionsPerRoot(cap int) ManagerOption {
	return func(m *Manager) {
		m.maxAsyncMissionsPerRoot = cap
	}
}

// WithDefaultInquireTimeoutMs sets the per-call session:inquire
// deadline used when the model omits timeout_ms and as the
// upper-bound clamp for caller-supplied timeouts. 0 leaves the
// pkg/session package-level fallback (1 hour) in place. Phase 5.1
// § 2.7.
func WithDefaultInquireTimeoutMs(ms int) ManagerOption {
	return func(m *Manager) {
		m.defaultInquireTimeoutMs = ms
	}
}

// WithTierIntents sets the per-tier model-router intent defaults
// (root / mission / worker → intent name) the runtime applies to
// spawned children before per-role overrides. Phase 4.2.2 §11.
func WithTierIntents(intents map[string]string) ManagerOption {
	return func(m *Manager) {
		if len(intents) == 0 {
			return
		}
		if m.tierIntents == nil {
			m.tierIntents = make(map[string]string, len(intents))
		}
		for k, v := range intents {
			m.tierIntents[k] = v
		}
	}
}

// WithTierDefaults threads the per-tier turn-loop budget defaults
// (root / mission / worker) into the Deps bundle every Session in
// the tree sees. Phase 5.2 δ (B3 migration). Defensively copied
// so the caller may not mutate the active map.
func WithTierDefaults(defaults map[string]session.TierTurnDefaults) ManagerOption {
	return func(m *Manager) {
		if len(defaults) == 0 {
			return
		}
		if m.tierDefaults == nil {
			m.tierDefaults = make(map[string]session.TierTurnDefaults, len(defaults))
		}
		for k, v := range defaults {
			m.tierDefaults[k] = v
		}
	}
}

// WithMaxParkedChildrenPerRoot caps the simultaneously-parked
// children across a root's subtree. 0 disables enforcement; the
// manager defaults to 3 when this option is omitted. Phase 5.2 ε.
func WithMaxParkedChildrenPerRoot(cap int) ManagerOption {
	return func(m *Manager) {
		m.maxParkedChildrenPerRoot = cap
	}
}

// WithParkedIdleTimeout sets the per-child idle deadline applied
// on park. 0 disables the timer; the manager defaults to 10
// minutes when this option is omitted. Phase 5.2 ε.
func WithParkedIdleTimeout(d time.Duration) ManagerOption {
	return func(m *Manager) {
		m.parkedIdleTimeout = d
	}
}

// NewManager constructs the manager. All required deps are
// passed in (constitution principle II). The manager owns a root
// context (separate from any adapter's errgroup context) that scopes
// every session goroutine; Shutdown cancels it.
func NewManager(
	store store.RuntimeStore,
	agent *session.Agent,
	models *model.ModelRouter,
	commands *session.CommandRegistry,
	codec *protocol.Codec,
	tools *tool.ToolManager,
	logger *slog.Logger,
	opts ...ManagerOption,
) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	if tools == nil {
		panic("session/manager: NewManager requires a non-nil *tool.ToolManager")
	}
	rootCtx, rootCancel := context.WithCancel(context.Background())
	m := &Manager{
		store:                    store,
		agent:                    agent,
		models:                   models,
		commands:                 commands,
		codec:                    codec,
		tools:                    tools,
		logger:                   logger,
		rootCtx:                  rootCtx,
		rootCancel:               rootCancel,
		live:                     make(map[string]*session.Session),
		maxAsyncMissionsPerRoot:  defaultMaxAsyncMissionsPerRoot,
		maxParkedChildrenPerRoot: defaultMaxParkedChildrenPerRoot,
		parkedIdleTimeout:        defaultParkedIdleTimeout,
	}
	for _, o := range opts {
		o(m)
	}
	// Build the shared Deps view AFTER the options ran, so
	// SessionOption / Extension updates picked up by m.sessionOpts /
	// m.extensions are reflected in the bundle that newSession /
	// newSessionRestore see.
	m.deps = &session.Deps{
		Store:               m.store,
		Agent:               m.agent,
		Models:              m.models,
		Commands:            m.commands,
		Codec:               m.codec,
		Tools:               m.tools,
		Logger:              m.logger,
		Prompts:             m.prompts,
		Extensions:          m.extensions,
		Opts:                m.sessionOpts,
		RootCtx:             m.rootCtx,
		WG:                  &m.wg,
		MaxDepth:                 session.DefaultMaxDepth,
		DefaultMissionSkill:      m.defaultMissionSkill,
		TierIntents:              m.tierIntents,
		TierDefaults:             m.tierDefaults,
		MaxAsyncMissionsPerRoot:  m.maxAsyncMissionsPerRoot,
		MaxParkedChildrenPerRoot: m.maxParkedChildrenPerRoot,
		ParkedIdleTimeout:        m.parkedIdleTimeout,
		DefaultInquireTimeoutMs:  m.defaultInquireTimeoutMs,
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
		// Track terminate goroutines in m.wg so Manager.Stop waits
		// for them before returning. Without this, a close-storm
		// during shutdown leaves Terminate goroutines running past
		// Stop with no observer; rootCancel propagates to the
		// targeted session anyway, so the goroutine eventually
		// exits via <-s.Done(), but Stop's caller sees the binary
		// "settled" prematurely.
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			_ = m.Terminate(context.WithoutCancel(ctx), id, reason)
		}()
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
func (m *Manager) Open(ctx context.Context, req session.OpenRequest) (*session.Session, time.Time, error) {
	s, err := session.New(ctx, m.deps, req)
	if err != nil {
		return nil, time.Time{}, err
	}
	m.mu.Lock()
	if existing, ok := m.live[s.ID()]; ok {
		// Race is theoretical for roots (id is random); keep the
		// branch defensive so a duplicate id can't leak goroutines.
		m.mu.Unlock()
		s.Discard()
		return existing, existing.OpenedAt(), nil
	}
	m.live[s.ID()] = s
	m.mu.Unlock()
	// Phase 4.1b-pre stage B: roots are removed from m.live by
	// Manager.Terminate after the session's Done channel closes.
	// Graceful shutdown leaves stale entries until the next process
	// boot — m.live is in-memory only and the binary is exiting.
	s.Start(ctx)
	return s, s.OpenedAt(), nil
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
func (m *Manager) Resume(ctx context.Context, id string) (*session.Session, error) {
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

	s, err := session.NewRestore(ctx, id, m.deps)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	if existing, ok := m.live[id]; ok {
		m.mu.Unlock()
		s.Discard()
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
		return store.ErrSessionNotFound
	}
	if s.IsClosed() {
		return ErrSessionGone
	}
	<-s.Submit(ctx, f)
	if s.IsClosed() {
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
	targets := make([]*session.Session, 0, len(m.live))
	for _, s := range m.live {
		targets = append(targets, s)
	}
	m.mu.RUnlock()
	for _, s := range targets {
		marker := protocol.NewSystemMarker(s.ID(), m.agent.Participant(), subject, meta)
		// Fire-and-forget broadcast — the per-session Submit
		// goroutine handles delivery + drop on closed inbox without
		// blocking the broadcast loop on any single slow session.
		_ = s.Submit(ctx, marker)
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
		return store.ErrSessionNotFound
	}
	closeFrame := protocol.NewSessionClose(s.ID(), m.agent.Participant(), reason)
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
func (m *Manager) ListSessions(ctx context.Context, status string) ([]session.SessionSummary, error) {
	rows, err := m.store.ListSessions(ctx, m.agent.ID(), status)
	if err != nil {
		return nil, err
	}
	return rowsToSummaries(rows), nil
}

// ListResumableRoots returns summaries of every root session for
// this agent whose status column is Active. Ordered by updated_at
// DESC. Backed by [store.RuntimeStore.ListResumableRoots]; the
// returned rows include their latest lifecycle event but adapters
// asking for a summary list don't need it, so we drop it here.
// Used by the console adapter's resume picker; RestoreActive calls
// the store directly so it can read the lifecycle classifier.
func (m *Manager) ListResumableRoots(ctx context.Context) ([]session.SessionSummary, error) {
	rows, err := m.store.ListResumableRoots(ctx, m.agent.ID())
	if err != nil {
		return nil, err
	}
	plain := make([]store.SessionRow, len(rows))
	for i, r := range rows {
		plain[i] = r.SessionRow
	}
	return rowsToSummaries(plain), nil
}

// SessionStats proxies to the underlying RuntimeStore. Phase
// 5.1c S2 — feeds the TUI adapter's footer indicator.
func (m *Manager) SessionStats(ctx context.Context, sessionID string) (int, error) {
	return m.store.SessionStats(ctx, sessionID)
}

// ListEvents proxies to the underlying RuntimeStore. Phase 5.1c —
// the TUI adapter calls this on tab attach to fetch the recent
// event log and replay it into the chat pane (on-attach rendering
// per §9). Public so AdapterHost can expose it without leaking
// the store interface.
func (m *Manager) ListEvents(ctx context.Context, sessionID string, opts store.ListEventsOpts) ([]store.EventRow, error) {
	return m.store.ListEvents(ctx, sessionID, opts)
}

func rowsToSummaries(rows []store.SessionRow) []session.SessionSummary {
	out := make([]session.SessionSummary, 0, len(rows))
	for _, r := range rows {
		out = append(out, session.SessionSummary{
			ID:        r.ID,
			Status:    r.Status,
			OpenedAt:  r.CreatedAt,
			UpdatedAt: r.UpdatedAt,
			Metadata:  r.Metadata,
		})
	}
	return out
}

// Get returns a live *Session by id (already-running). Used by the
// supervisor goroutine to route inbound frames.
func (m *Manager) Get(id string) (*session.Session, bool) {
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
