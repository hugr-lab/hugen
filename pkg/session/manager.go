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
type OpenRequest struct {
	OwnerID      string
	Participants []protocol.ParticipantInfo
	// Metadata is persisted verbatim on the session row. Adapters
	// validate size/shape before passing it through; the manager
	// stores it as-is.
	Metadata map[string]any
}

// Manager owns the live *Session map and brokers
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

type Manager struct {
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
	// wg tracks every spawned session goroutine. ShutdownAll uses
	// it to wait for every goroutine to finish writing its
	// terminal status BEFORE the local DuckDB engine closes —
	// without this guarantee an in-flight UPDATE races the engine
	// teardown.
	wg sync.WaitGroup
}

// ManagerOption configures a Manager at construction.
type ManagerOption func(*Manager)

// WithLifecycle attaches OnOpen/OnClose hooks to the manager.
// Used by cmd/hugen to wire per-session bash-mcp lifecycle without
// pulling tool/skill/permission imports into pkg/runtime.
func WithLifecycle(l SessionLifecycle) ManagerOption {
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
	return m
}

// Open creates a fresh session row, builds an in-memory *Session,
// starts its goroutine, and emits a session_opened frame. Returns
// the session and the row's CreatedAt timestamp so callers can
// echo the persisted opened_at without an extra LoadSession.
func (m *Manager) Open(ctx context.Context, req OpenRequest) (*Session, time.Time, error) {
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
func (m *Manager) Resume(ctx context.Context, id string) (*Session, error) {
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

// Close transitions the session to "closed" and waits for the
// session's goroutine to finish writing the terminal status. The
// goroutine itself is the only writer to the row (single-writer
// invariant) — Manager pushes a SessionClosed intent frame into
// the inbox and blocks on s.Done until the goroutine has run
// MarkClosed and exited.
//
// Idempotent: a session that's already terminated returns the
// stored closed_at. Returns ErrSessionNotFound if the session
// doesn't exist in the store either.
func (m *Manager) Close(ctx context.Context, id, reason string) (time.Time, error) {
	m.mu.RLock()
	s, live := m.live[id]
	m.mu.RUnlock()

	if !live {
		// No live goroutine to forward to. Status update on
		// already-suspended rows happens via a different code
		// path (resume + close), out of scope here. We just read
		// the existing row and report.
		row, err := m.store.LoadSession(ctx, id)
		if err != nil {
			return time.Time{}, err
		}
		if row.Status == StatusClosed {
			return row.UpdatedAt, nil
		}
		// Session is in the store but not live (suspended). The
		// goroutine isn't around to enforce the single-writer
		// invariant — fall back to a direct update, which is
		// safe because nobody else is touching the row.
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

	if !s.closed.Load() {
		closed := protocol.NewSessionClosed(id, m.agent.Participant(), reason)
		if !s.Submit(ctx, closed) {
			m.logger.Warn("manager: session inbox closed before Close intent landed",
				"session", id)
		}
	}
	// Wait for the session's goroutine to flush its terminal
	// status and exit. Hard cap on ctx so a stuck handler can't
	// pin the API caller indefinitely.
	select {
	case <-s.Done():
	case <-ctx.Done():
		return time.Time{}, ctx.Err()
	}
	if m.lifecycle.OnClose != nil {
		if err := m.lifecycle.OnClose(ctx, id); err != nil {
			m.logger.Warn("manager: close session lifecycle", "session", id, "err", err)
		}
	}
	return time.Now().UTC(), nil
}

// Suspend asks the session goroutine to record `suspended`
// status. Implemented as a thin wrapper around the inbox-frame
// dispatch so the single-writer invariant holds — Manager never
// UPDATEs a sessions row directly when a goroutine owns it.
//
// Returns immediately after pushing the intent. Status is
// recorded asynchronously by the session goroutine on its next
// turn boundary; subsequent calls observe it through the store.
// If no live goroutine is around (suspended or never spawned),
// the row is updated directly — there is no goroutine that could
// race the write.
func (m *Manager) Suspend(ctx context.Context, id string) error {
	m.mu.RLock()
	s, live := m.live[id]
	m.mu.RUnlock()
	if !live {
		return m.store.UpdateSessionStatus(ctx, id, StatusSuspended)
	}
	if s.closed.Load() {
		return nil
	}
	marker := protocol.NewSessionSuspended(id, m.agent.Participant())
	if !s.Submit(ctx, marker) {
		m.logger.Warn("manager: session inbox closed before Suspend intent landed",
			"session", id)
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

// ShutdownAll asks every live session to suspend, then waits for
// their goroutines to exit. Single-writer ordering: the session
// goroutine is the only writer to its sessions row; here we just
// push intents and wait. Only after every wg.Done has fired do
// we cancel the root context — that way any in-flight UPDATE
// finished against a still-open store before downstream tear-down
// (Tools.Close, LocalEngine.Close) starts.
//
// Idempotent and safe to call multiple times.
func (m *Manager) ShutdownAll(ctx context.Context) {
	m.mu.Lock()
	live := make([]*Session, 0, len(m.live))
	for _, s := range m.live {
		live = append(live, s)
	}
	m.mu.Unlock()
	for _, s := range live {
		if !s.closed.Load() {
			marker := protocol.NewSessionSuspended(s.id, m.agent.Participant())
			_ = s.Submit(ctx, marker)
		}
		// close(s.in) lets the Run loop exit after draining any
		// already-queued frames. The session's own /end-in-flight,
		// if any, runs to completion before this empty inbox is
		// observed.
		func() {
			defer func() { _ = recover() }()
			close(s.in)
		}()
	}
	// Wait for every session goroutine to finish writing terminal
	// status and exit. Goroutines self-deregister from m.live in
	// their defer chain, so by the time wg.Wait returns m.live is
	// empty.
	m.wg.Wait()
	m.rootCancel()
}

// spawn registers a new live Session and starts its goroutine.
// The session goroutine runs against m.rootCtx so it survives the
// caller's context (typically an adapter's errgroup context).
//
// Re-checks live[id] under the write lock so concurrent Open/Resume
// callers can't double-spawn an orphan goroutine.
func (m *Manager) spawn(_ context.Context, id string) *Session {
	s := NewSession(id, m.agent, m.store, m.models, m.commands, m.codec, m.logger, m.sessionOpts...)
	m.mu.Lock()
	if existing, ok := m.live[id]; ok {
		m.mu.Unlock()
		return existing
	}
	m.live[id] = s
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.wg.Done()
		// Self-deregister on exit so Manager.Get / Snapshot
		// reflect the live state without an external write.
		// Done strictly after the Run loop returns, so by the
		// time another goroutine looks up id == not-live, the
		// terminal status is already in the store.
		defer func() {
			m.mu.Lock()
			if cur, ok := m.live[id]; ok && cur == s {
				delete(m.live, id)
			}
			m.mu.Unlock()
		}()
		if err := s.Run(m.rootCtx); err != nil && !errors.Is(err, context.Canceled) {
			m.logger.Warn("session loop exited", "session", id, "err", err)
		}
	}()
	return s
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
