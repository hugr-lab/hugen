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
//   - ParentSessionID, Depth, SessionType, SpawnedFromEventID — set by
//     Manager.Spawn for sub-agent sessions; left zero for root
//     sessions opened by adapters.
type OpenRequest struct {
	OwnerID      string
	Participants []protocol.ParticipantInfo
	// Metadata is persisted verbatim on the session row. Adapters
	// validate size/shape before passing it through; the manager
	// stores it as-is. For sub-agents the manager also writes
	// metadata["depth"] (set to parent.depth+1, immutable) here.
	Metadata map[string]any

	ParentSessionID    string
	Depth              int
	SessionType        string
	SpawnedFromEventID string
}

// SpawnSpec is the input to Manager.Spawn. Carries the model-supplied
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

	rootCtx    context.Context
	rootCancel context.CancelFunc

	mu   sync.RWMutex
	live map[string]*Session
	// cancels keys per-session ctx cancellation by session id, set by
	// spawn(). Used by Terminate to cancel one session without
	// affecting siblings; by ShutdownAll only via rootCancel
	// (which propagates to every per-session ctx via context derivation).
	cancels map[string]context.CancelCauseFunc
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
		cancels:    make(map[string]context.CancelCauseFunc),
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Open creates a fresh session row, builds an in-memory *Session,
// starts its goroutine, and emits a session_opened frame. Returns
// the session and the row's CreatedAt timestamp so callers can
// echo the persisted opened_at without an extra LoadSession.
//
// Phase 4: req.ParentSessionID/Depth/SessionType/SpawnedFromEventID
// are written to the row when set. Sub-agents reach this code path
// via Manager.Spawn; root sessions leave those fields zero so
// SessionType defaults to "root".
func (m *Manager) Open(ctx context.Context, req OpenRequest) (*Session, time.Time, error) {
	id := newSessionID()
	now := time.Now().UTC()
	sessionType := req.SessionType
	if sessionType == "" {
		sessionType = "root"
	}
	row := SessionRow{
		ID:                 id,
		AgentID:            m.agent.ID(),
		OwnerID:            req.OwnerID,
		ParentSessionID:    req.ParentSessionID,
		SessionType:        sessionType,
		SpawnedFromEventID: req.SpawnedFromEventID,
		Status:             StatusActive,
		Metadata:           req.Metadata,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	if err := m.store.OpenSession(ctx, row); err != nil {
		return nil, time.Time{}, fmt.Errorf("manager: open session: %w", err)
	}
	if m.lifecycle != nil {
		if err := m.lifecycle.Acquire(ctx, id); err != nil {
			// Phase-4 strict event-sourcing: the sessions row is
			// immutable after create. Mark the orphaned session
			// terminal via a session_terminated event so the restart
			// walker on next boot doesn't try to resume it.
			terminal := protocol.NewSessionTerminated(id, m.agent.Participant(), protocol.SessionTerminatedPayload{
				Reason: "acquire_failed",
			})
			if termRow, summary, perr := FrameToEventRow(terminal, m.agent.ID()); perr == nil {
				_ = m.store.AppendEvent(ctx, termRow, summary)
			}
			return nil, time.Time{}, fmt.Errorf("manager: open session lifecycle: %w", err)
		}
	}
	s := m.spawn(ctx, id)
	// Mark the new session as "materialised already" — there's no
	// prior history to walk.
	s.materialised.Store(true)

	parts := req.Participants
	if len(parts) == 0 {
		parts = []protocol.ParticipantInfo{m.agent.Participant()}
	}
	opened := protocol.NewSessionOpened(id, m.agent.Participant(), parts)
	if err := s.emit(ctx, opened); err != nil {
		m.logger.Error("manager: emit session_opened", "session", id, "err", err)
	}
	return s, now, nil
}

// Resume reattaches to an existing session row. Materialisation is
// deferred to the first inbound Frame after resume.
//
// Concurrent calls for the same id will share the same *Session —
// the spawn-side double-check guarantees no orphan goroutine. Only
// the first caller observes the session_resumed marker.
func (m *Manager) Resume(ctx context.Context, id string) (*Session, error) {
	row, err := m.store.LoadSession(ctx, id)
	if err != nil {
		return nil, err
	}
	if isSessionTerminated(ctx, m.store, id) {
		return nil, ErrSessionClosed
	}
	m.mu.RLock()
	if existing, ok := m.live[id]; ok {
		m.mu.RUnlock()
		return existing, nil
	}
	m.mu.RUnlock()
	// Re-run Acquire so per-session resources reattach after a
	// process restart: per_session MCPs are respawned, autoload
	// skills re-bound, workspace dir re-prepared. Resources.Acquire
	// is idempotent (MkdirAll, autoload-Load and AddSessionProvider
	// all tolerate a no-op when state is already correct).
	if m.lifecycle != nil {
		if err := m.lifecycle.Acquire(ctx, id); err != nil {
			m.logger.Warn("manager: resume lifecycle", "session", id, "err", err)
		}
	}
	s := m.spawn(ctx, id)
	// Only emit the resume marker if spawn actually created a fresh
	// goroutine (i.e. we won the race). Compare by pointer identity.
	m.mu.RLock()
	current := m.live[id]
	m.mu.RUnlock()
	if current == s {
		marker := protocol.NewSystemMarker(id, m.agent.Participant(), "session_resumed",
			map[string]any{"prior_status": row.Status})
		if err := s.emit(ctx, marker); err != nil {
			m.logger.Warn("manager: emit session_resumed marker", "session", id, "err", err)
		}
	}
	return s, nil
}

// Spawn opens a sub-agent session as a child of parent. The child row
// is written via Open with session_type="subagent", parent_session_id
// set, and metadata["depth"] = parent.depth+1 (immutable after create).
// A subagent_started event is appended to the **parent's** events
// carrying the child id, role, task, depth, started_at, optional
// inputs, and the captured parent_whiteboard_active flag. Caller is
// responsible for permission / depth-ceiling / role.can_spawn checks
// (those land in commit 10).
//
// Returns the child *Session (already running its goroutine) so the
// caller can hold a reference if needed; the subagent_result Frame
// will be delivered to parent's inbox by the child goroutine on exit.
func (m *Manager) Spawn(ctx context.Context, parent *Session, spec SpawnSpec) (*Session, error) {
	if parent == nil {
		return nil, fmt.Errorf("manager: spawn requires parent session")
	}
	// Load parent row to read depth + owner. Cheap because the row
	// is hot in the local store; phase-4 makes the row immutable so
	// caching is also safe but unnecessary for spawn.
	parentRow, err := m.store.LoadSession(ctx, parent.ID())
	if err != nil {
		return nil, fmt.Errorf("manager: spawn load parent: %w", err)
	}
	parentDepth := 0
	if d, ok := parentRow.Metadata["depth"].(int); ok {
		parentDepth = d
	} else if df, ok := parentRow.Metadata["depth"].(float64); ok {
		parentDepth = int(df)
	}
	childDepth := parentDepth + 1
	childMeta := map[string]any{
		"depth":       childDepth,
		"spawn_role":  spec.Role,
		"spawn_skill": spec.Skill,
	}
	for k, v := range spec.Metadata {
		childMeta[k] = v
	}
	child, openedAt, err := m.Open(ctx, OpenRequest{
		OwnerID:            parentRow.OwnerID,
		ParentSessionID:    parent.ID(),
		Depth:              childDepth,
		SessionType:        "subagent",
		SpawnedFromEventID: spec.EventID,
		Metadata:           childMeta,
	})
	if err != nil {
		return nil, fmt.Errorf("manager: spawn open: %w", err)
	}
	// Append subagent_started to the parent's events. parent.emit
	// allocates the next per-session seq + persists, like every other
	// frame. The frame is also fanned out to parent.out so adapters
	// observe the spawn live.
	started := protocol.NewSubagentStarted(parent.ID(), m.agent.Participant(), protocol.SubagentStartedPayload{
		ChildSessionID:         child.ID(),
		Skill:                  spec.Skill,
		Role:                   spec.Role,
		Task:                   spec.Task,
		Depth:                  childDepth,
		StartedAt:              openedAt,
		Inputs:                 spec.Inputs,
		ParentWhiteboardActive: spec.ParentWhiteboardActive,
	})
	if err := parent.emit(ctx, started); err != nil {
		m.logger.Warn("manager: emit subagent_started", "parent", parent.ID(), "child", child.ID(), "err", err)
	}
	return child, nil
}

// Deliver pushes a Frame onto the addressed session's inbox. Recover-
// safe wrapper over Session.Submit so cross-session frame delivery
// has a single audit-friendly entry point and so panics from a
// goroutine racing its own exit don't propagate.
//
// Used by phase-4 cross-session paths: a child emitting subagent_result
// to its parent, a sub-agent's whiteboard_write Frame to its host
// parent, the host's whiteboard_message broadcast to siblings, and
// (phase 5) HITL chain forwarding.
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
	cancel, hasCancel := m.cancels[id]
	m.mu.RUnlock()
	if live && hasCancel {
		if !s.closed.Load() {
			cancel(&terminationCause{reason: reason, emitClose: true})
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

// List returns lightweight summaries of every session row for this
// agent.
func (m *Manager) List(ctx context.Context, status string) ([]SessionSummary, error) {
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

// spawn registers a new live Session and starts its goroutine.
// Each session runs against a per-session ctx derived from m.rootCtx
// via WithCancelCause, so Terminate can cancel one session in
// isolation while ShutdownAll cancels every session at once via
// rootCancel.
//
// Re-checks live[id] under the write lock so concurrent Open/Resume
// callers can't double-spawn an orphan goroutine.
func (m *Manager) spawn(_ context.Context, id string) *Session {
	s := NewSession(id, m.agent, m.store, m.models, m.commands, m.codec, m.logger, m.sessionOpts...)
	sessCtx, cancel := context.WithCancelCause(m.rootCtx)
	m.mu.Lock()
	if existing, ok := m.live[id]; ok {
		m.mu.Unlock()
		cancel(nil) // race lost; release the ctx we created
		return existing
	}
	m.live[id] = s
	m.cancels[id] = cancel
	s.terminate = cancel
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.wg.Done()
		// Self-deregister on exit so Manager.Get reflects the live
		// state without an external write. Done strictly after the
		// Run loop returns, so by the time another goroutine looks
		// up id == not-live, any session_terminated event is
		// already in the store.
		defer func() {
			m.mu.Lock()
			if cur, ok := m.live[id]; ok && cur == s {
				delete(m.live, id)
			}
			delete(m.cancels, id)
			m.mu.Unlock()
		}()
		if err := s.Run(sessCtx); err != nil && !errors.Is(err, context.Canceled) {
			m.logger.Warn("session loop exited", "session", id, "err", err)
		}
	}()
	return s
}

// isSessionTerminated reports whether the session id has a
// session_terminated event in its events. Phase-4 liveness is
// event-derived (FR-027); the legacy `sessions.status` column is
// pinned to 'active' at create and never updated.
func isSessionTerminated(ctx context.Context, store RuntimeStore, id string) bool {
	events, err := store.ListEvents(ctx, id, ListEventsOpts{Limit: 1000})
	if err != nil {
		return false
	}
	for _, ev := range events {
		if ev.EventType == string(protocol.KindSessionTerminated) {
			return true
		}
	}
	return false
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
