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
	UpdatedAt time.Time
}

// OpenRequest carries the parameters for SessionManager.Open.
type OpenRequest struct {
	OwnerID      string
	Participants []protocol.ParticipantInfo
}

// SessionManager owns the live *Session map and brokers
// open/resume/close. Each Session runs in its own goroutine.
//
// Sessions outlive the adapter goroutines that opened them: if an
// adapter exits cleanly, the session goroutine keeps running until
// either /end fires or the runtime is shut down explicitly. This
// is what makes the long-lived-session promise honest — adapter
// crash != session loss.
type SessionManager struct {
	store    RuntimeStore
	agent    *Agent
	models   *model.ModelRouter
	commands *CommandRegistry
	codec    *protocol.Codec
	logger   *slog.Logger

	rootCtx    context.Context
	rootCancel context.CancelFunc

	mu   sync.RWMutex
	live map[string]*Session
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
) *SessionManager {
	if logger == nil {
		logger = slog.Default()
	}
	rootCtx, rootCancel := context.WithCancel(context.Background())
	return &SessionManager{
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
}

// Open creates a fresh session row, builds an in-memory *Session,
// starts its goroutine, and emits a session_opened frame.
func (m *SessionManager) Open(ctx context.Context, req OpenRequest) (*Session, error) {
	id := newSessionID()
	now := time.Now().UTC()
	row := SessionRow{
		ID:          id,
		AgentID:     m.agent.ID(),
		OwnerID:     req.OwnerID,
		SessionType: "root",
		Status:      StatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := m.store.OpenSession(ctx, row); err != nil {
		return nil, fmt.Errorf("manager: open session: %w", err)
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
	return s, nil
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
// goroutine. Idempotent on already-closed sessions.
func (m *SessionManager) Close(ctx context.Context, id, reason string) error {
	m.mu.Lock()
	s, ok := m.live[id]
	if ok {
		delete(m.live, id)
	}
	m.mu.Unlock()
	if ok && !s.closed.Load() {
		closed := protocol.NewSessionClosed(id, m.agent.Participant(), reason)
		if err := s.emit(ctx, closed); err != nil {
			m.logger.Warn("manager: emit session_closed", "session", id, "err", err)
		}
		close(s.in)
	}
	if err := m.store.UpdateSessionStatus(ctx, id, StatusClosed); err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return nil
		}
		return err
	}
	return nil
}

// Suspend updates the row to suspended without ending the goroutine.
// Used during graceful shutdown. Skips the in-band emit if the
// session has already closed (e.g. /end fired before shutdown).
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
	if ok && s.closed.Load() {
		// Already closed — nothing more to mutate.
		return nil
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
		out = append(out, SessionSummary{ID: r.ID, Status: r.Status, UpdatedAt: r.UpdatedAt})
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
	s := NewSession(id, m.agent, m.store, m.models, m.commands, m.codec, m.logger)
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
