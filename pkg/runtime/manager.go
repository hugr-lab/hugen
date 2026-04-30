package runtime

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

// OpenRequest carries the parameters for SessionManager.Open.
type OpenRequest struct {
	OwnerID      string
	Participants []protocol.ParticipantInfo
	// Metadata is persisted verbatim on the session row. Adapters
	// validate size/shape before passing it through; the manager
	// stores it as-is.
	Metadata map[string]any
}

// SessionManager owns the live *Session map and brokers
// open/resume/close. Each Session runs in its own goroutine.
//
// Sessions outlive the adapter goroutines that opened them: if an
// adapter exits cleanly, the session goroutine keeps running until
// either /end fires or the runtime is shut down explicitly. This
// is what makes the long-lived-session promise honest — adapter
// crash != session loss.
// SessionLifecycle is an optional hook the runtime calls on
// Session.Open and Session.Close. Used by cmd/hugen to spawn /
// teardown per-session resources (the workspace directory and
// the per-session bash-mcp subprocess + tool.MCPProvider). All
// methods may be nil.
//
// Hooks run synchronously inside Open/Close; an OnOpen error
// fails the Open and rolls back the session row. OnClose errors
// are logged but do not fail Close (the session row is already
// transitioning to closed).
type SessionLifecycle struct {
	OnOpen  func(ctx context.Context, sessionID string) error
	OnClose func(ctx context.Context, sessionID string) error
}

type SessionManager struct {
	store    RuntimeStore
	agent    *Agent
	models   *model.ModelRouter
	commands *CommandRegistry
	codec    *protocol.Codec
	logger   *slog.Logger

	sessionOpts []SessionOption
	lifecycle   SessionLifecycle

	rootCtx    context.Context
	rootCancel context.CancelFunc

	mu   sync.RWMutex
	live map[string]*Session
}

// SessionManagerOption configures a SessionManager at construction.
type SessionManagerOption func(*SessionManager)

// WithLifecycle attaches OnOpen/OnClose hooks to the manager.
// Used by cmd/hugen to wire per-session bash-mcp lifecycle without
// pulling tool/skill/permission imports into pkg/runtime.
func WithLifecycle(l SessionLifecycle) SessionManagerOption {
	return func(m *SessionManager) { m.lifecycle = l }
}

// WithSessionOptions threads SessionOption values through every
// spawned Session — typically used by cmd/hugen to attach the
// shared *tool.ToolManager via WithTools.
func WithSessionOptions(opts ...SessionOption) SessionManagerOption {
	return func(m *SessionManager) {
		m.sessionOpts = append(m.sessionOpts, opts...)
	}
}

// NewSessionManager constructs the manager. All required deps are
// passed in (constitution principle II). The manager owns a root
// context (separate from any adapter's errgroup context) that scopes
// every session goroutine; Shutdown cancels it.
func NewSessionManager(
	store RuntimeStore,
	agent *Agent,
	models *model.ModelRouter,
	commands *CommandRegistry,
	codec *protocol.Codec,
	logger *slog.Logger,
	opts ...SessionManagerOption,
) *SessionManager {
	if logger == nil {
		logger = slog.Default()
	}
	rootCtx, rootCancel := context.WithCancel(context.Background())
	m := &SessionManager{
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
	return m
}

// Open creates a fresh session row, builds an in-memory *Session,
// starts its goroutine, and emits a session_opened frame. Returns
// the session and the row's CreatedAt timestamp so callers can
// echo the persisted opened_at without an extra LoadSession.
func (m *SessionManager) Open(ctx context.Context, req OpenRequest) (*Session, time.Time, error) {
	id := newSessionID()
	now := time.Now().UTC()
	row := SessionRow{
		ID:          id,
		AgentID:     m.agent.ID(),
		OwnerID:     req.OwnerID,
		SessionType: "root",
		Status:      StatusActive,
		Metadata:    req.Metadata,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := m.store.OpenSession(ctx, row); err != nil {
		return nil, time.Time{}, fmt.Errorf("manager: open session: %w", err)
	}
	if m.lifecycle.OnOpen != nil {
		if err := m.lifecycle.OnOpen(ctx, id); err != nil {
			// Roll back the session row so a failed lifecycle hook
			// (e.g. workspace mkdir or bash-mcp spawn) doesn't leave
			// an orphan active row.
			_ = m.store.UpdateSessionStatus(ctx, id, StatusClosed)
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
func (m *SessionManager) Resume(ctx context.Context, id string) (*Session, error) {
	row, err := m.store.LoadSession(ctx, id)
	if err != nil {
		return nil, err
	}
	if row.Status == StatusClosed {
		return nil, ErrSessionClosed
	}
	m.mu.RLock()
	if existing, ok := m.live[id]; ok {
		m.mu.RUnlock()
		return existing, nil
	}
	m.mu.RUnlock()

	// Re-mark active if we're resuming from suspended.
	if row.Status == StatusSuspended {
		if err := m.store.UpdateSessionStatus(ctx, id, StatusActive); err != nil {
			m.logger.Warn("manager: re-activate session", "session", id, "err", err)
		}
	}
	// Re-run the OnOpen hook so per-session resources reattach
	// after a process restart: bash-mcp (and other per_session MCPs)
	// must be respawned, autoload skills re-bound, workspace dir
	// re-prepared. The hook is idempotent — MkdirAll, autoload-Load
	// and AddSessionProvider all tolerate a no-op when state is
	// already correct.
	if m.lifecycle.OnOpen != nil {
		if err := m.lifecycle.OnOpen(ctx, id); err != nil {
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

// Close transitions the session to "closed" and tears down its
// goroutine. Idempotent on already-closed sessions: returns the
// original closed_at timestamp. Returns ErrSessionNotFound if the
// session does not exist on the store at all — the HTTP adapter
// maps that to 404.
func (m *SessionManager) Close(ctx context.Context, id, reason string) (time.Time, error) {
	m.mu.Lock()
	s, live := m.live[id]
	if live {
		delete(m.live, id)
	}
	m.mu.Unlock()

	// If not in m.live, the session is either suspended (still in
	// the store) or has never existed. LoadSession distinguishes
	// and also reveals the existing closed_at when applicable.
	var existing SessionRow
	var existsInStore bool
	if !live {
		row, err := m.store.LoadSession(ctx, id)
		if err != nil {
			return time.Time{}, err
		}
		existing = row
		existsInStore = true
	}

	if live && !s.closed.Load() {
		closed := protocol.NewSessionClosed(id, m.agent.Participant(), reason)
		if err := s.emit(ctx, closed); err != nil {
			m.logger.Warn("manager: emit session_closed", "session", id, "err", err)
		}
		close(s.in)
	}

	// Already closed in store → preserve the original timestamp.
	if existsInStore && existing.Status == StatusClosed {
		return existing.UpdatedAt, nil
	}
	if err := m.store.UpdateSessionStatus(ctx, id, StatusClosed); err != nil {
		return time.Time{}, err
	}
	if m.lifecycle.OnClose != nil {
		if err := m.lifecycle.OnClose(ctx, id); err != nil {
			m.logger.Warn("manager: close session lifecycle", "session", id, "err", err)
		}
	}
	return time.Now().UTC(), nil
}

// Suspend updates the row to suspended without ending the goroutine.
// Used during graceful shutdown. Skips the in-band emit if the
// session has already closed (e.g. /end fired before shutdown).
// Serialised against MarkClosed via Session.statusMu so concurrent
// /end + ShutdownAll don't collide on the same DuckDB row.
func (m *SessionManager) Suspend(ctx context.Context, id string) error {
	m.mu.RLock()
	s, ok := m.live[id]
	m.mu.RUnlock()
	if ok && !s.closed.Load() {
		marker := protocol.NewSessionSuspended(id, m.agent.Participant())
		if err := s.emit(ctx, marker); err != nil && !errors.Is(err, ErrSessionClosed) {
			m.logger.Warn("manager: emit session_suspended", "session", id, "err", err)
		}
	}
	if ok {
		s.statusMu.Lock()
		defer s.statusMu.Unlock()
		if s.closed.Load() {
			// Already closed — nothing more to mutate.
			return nil
		}
	}
	return m.store.UpdateSessionStatus(ctx, id, StatusSuspended)
}

// List returns lightweight summaries of every session row for this
// agent.
func (m *SessionManager) List(ctx context.Context, status string) ([]SessionSummary, error) {
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
func (m *SessionManager) Get(id string) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.live[id]
	return s, ok
}

// ShutdownAll suspends every live session, cancels the root context
// (so any blocked persistence call can unwind), and closes inboxes.
// Idempotent and safe to call multiple times.
func (m *SessionManager) ShutdownAll(ctx context.Context) {
	m.mu.Lock()
	live := make([]*Session, 0, len(m.live))
	for _, s := range m.live {
		live = append(live, s)
	}
	m.live = make(map[string]*Session)
	m.mu.Unlock()
	for _, s := range live {
		if !s.closed.Load() {
			_ = m.Suspend(ctx, s.id)
		}
		func() {
			defer func() { _ = recover() }()
			close(s.in)
		}()
	}
	m.rootCancel()
}

// spawn registers a new live Session and starts its goroutine.
// The session goroutine runs against m.rootCtx so it survives the
// caller's context (typically an adapter's errgroup context).
//
// Re-checks live[id] under the write lock so concurrent Open/Resume
// callers can't double-spawn an orphan goroutine.
func (m *SessionManager) spawn(_ context.Context, id string) *Session {
	s := NewSession(id, m.agent, m.store, m.models, m.commands, m.codec, m.logger, m.sessionOpts...)
	m.mu.Lock()
	if existing, ok := m.live[id]; ok {
		m.mu.Unlock()
		return existing
	}
	m.live[id] = s
	m.mu.Unlock()
	go func() {
		if err := s.Run(m.rootCtx); err != nil && !errors.Is(err, context.Canceled) {
			m.logger.Warn("session loop exited", "session", id, "err", err)
		}
	}()
	return s
}

// SessionsLive returns the IDs of currently live sessions.
func (m *SessionManager) SessionsLive() []string {
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
