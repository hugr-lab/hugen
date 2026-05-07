package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

// ErrDepthExceeded is returned by parent.Spawn when the new
// sub-agent's depth would exceed deps.MaxDepth.
var ErrDepthExceeded = errors.New("session: max sub-agent depth exceeded")

var (
	ErrSessionClosed   = store.ErrSessionClosed
	ErrSessionNotFound = store.ErrSessionNotFound
)

const (
	StatusActive    = store.StatusActive
	StatusSuspended = store.StatusSuspended // legacy; phase-4 never writes
	StatusClosed    = store.StatusClosed    // legacy; phase-4 never writes
)

type (
	EventRow       = store.EventRow
	SessionRow     = store.SessionRow
	ListEventsOpts = store.ListEventsOpts
	RuntimeStore   = store.RuntimeStore
)

var (
	EventRowToFrame = store.EventRowToFrame
	FrameToEventRow = store.FrameToEventRow
)

type (
	NoteRow = store.NoteRow
)

// New creates a fresh root Session. Thin public wrapper over the
// package-private newSession constructor; exists so the upcoming
// pkg/session/manager subpackage can construct roots without
// reaching into package internals.
//
// req.ParentSessionID must be empty — pass through NewChild for
// sub-agents (or use parent.Spawn, which keeps the children-map +
// SubagentStarted bookkeeping intact).
func New(ctx context.Context, deps *Deps, req OpenRequest) (*Session, error) {
	return newSession(ctx, nil, deps, req)
}

// NewChild creates a sub-agent Session as a child of parent. Public
// wrapper over newSession with a non-nil parent; the caller is
// responsible for any side effects Spawn normally performs
// (children-map registration, SubagentStarted emit, childWG.Add).
// Most production code should call parent.Spawn instead.
func NewChild(ctx context.Context, parent *Session, req OpenRequest) (*Session, error) {
	if parent == nil {
		return nil, fmt.Errorf("session: NewChild requires a parent")
	}
	return newSession(ctx, parent, parent.deps, req)
}

// NewRestore re-creates a root Session from an existing sessions
// row. Thin public wrapper over newSessionRestore; phase 4 only
// restores roots, so parent is implicitly nil.
func NewRestore(ctx context.Context, id string, deps *Deps) (*Session, error) {
	return newSessionRestore(ctx, id, nil, deps)
}

// newSession creates a fresh Session (root or sub-agent), writes its
// initial sessions row, runs lifecycle.Acquire, and emits a
// SessionOpened frame. The returned Session is ready for s.start.
//
//   - parent == nil → root: ctx derived from deps.RootCtx, depth = 0,
//     SessionType = "root".
//   - parent != nil → sub-agent: ctx derived from parent.ctx, depth =
//     parent.depth + 1, SessionType = "subagent". Caller is
//     responsible for the depth-ceiling check + permission gate before
//     calling newSession (it's an error if depth+1 exceeds maxDepth at
//     the spawn site).
//
// Failure path: if OpenSession fails the row is never persisted —
// caller cancels ctx and returns. If lifecycle.Acquire fails after
// the row was written, newSession appends a session_terminated
// {reason="acquire_failed"} event so the orphaned row is unambiguously
// terminal on next boot.
func newSession(ctx context.Context, parent *Session, deps *Deps, req OpenRequest) (*Session, error) {
	id := newSessionID()
	depth := 0
	sessionType := "root"
	var parentCtx context.Context
	if parent != nil {
		depth = parent.depth + 1
		sessionType = "subagent"
		parentCtx = parent.ctx
	} else {
		parentCtx = deps.RootCtx
	}
	if parentCtx == nil {
		// Defensive: should never happen post-pivot; deps.RootCtx is
		// always set by NewManager.
		parentCtx = context.Background()
	}
	sessCtx, cancel := context.WithCancelCause(parentCtx)

	// 1. Build the *Session shell (no IO yet — easier to roll back on
	// downstream errors below).
	s := buildSessionShell(id, depth, parent, deps, sessCtx, cancel)

	// 2. Persist the initial row (the only sessions-row WRITE on the
	// open path; future writes touch session_events instead).
	now := time.Now().UTC()
	row := store.SessionRow{
		ID:                 id,
		AgentID:            deps.Agent.ID(),
		OwnerID:            req.OwnerID,
		ParentSessionID:    req.ParentSessionID,
		SessionType:        sessionType,
		SpawnedFromEventID: req.SpawnedFromEventID,
		Status:             StatusActive,
		Metadata:           req.Metadata,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	if err := deps.Store.OpenSession(ctx, row); err != nil {
		cancel(nil)
		return nil, fmt.Errorf("session: open row: %w", err)
	}
	s.openedAt = now
	s.ownerID = req.OwnerID

	// 3. Lifecycle.Acquire (workspace dir, autoload skills, per_session
	// MCPs). Failure here means the row exists but no goroutine —
	// mark the row terminal so restart-walker doesn't try to resume
	// it.
	if deps.Lifecycle != nil {
		if err := deps.Lifecycle.Acquire(ctx, s); err != nil {
			s.appendTerminal(ctx, "acquire_failed")
			cancel(nil)
			return nil, fmt.Errorf("session: acquire: %w", err)
		}
	}

	// 3a. Extension InitState — let every registered extension stash
	// its per-session typed handle in s state via SetValue. Order is
	// preserved: a later extension can read state an earlier one
	// stored. Errors are logged and the session continues — extensions
	// must be best-effort at startup; tool dispatch surfaces specific
	// failures later if the handle is missing.
	for _, ext := range deps.Extensions {
		init, ok := ext.(extension.StateInitializer)
		if !ok {
			continue
		}
		if err := init.InitState(ctx, s); err != nil {
			deps.Logger.Warn("session: extension InitState",
				"session", id, "extension", ext.Name(), "err", err)
		}
	}

	// 4. Emit SessionOpened so adapters / event log carry the live-cycle
	// marker. Only roots get a SessionOpened — sub-agents are signalled
	// to the parent's events via the SubagentStarted frame the caller
	// (parent.Spawn) emits next.
	if parent == nil {
		parts := req.Participants
		if len(parts) == 0 {
			parts = []protocol.ParticipantInfo{deps.Agent.Participant()}
		}
		opened := protocol.NewSessionOpened(id, deps.Agent.Participant(), parts)
		if err := s.emit(ctx, opened); err != nil && !errors.Is(err, ErrSessionClosed) {
			deps.Logger.Warn("session: emit session_opened", "session", id, "err", err)
		}
	}

	// Mark in-memory materialise flag so the first inbound Frame
	// doesn't trigger a redundant store walk for a session with no
	// prior history.
	s.materialised.Store(true)

	return s, nil
}

// newSessionRestore re-creates a Session from an existing sessions
// row. Used by Manager.Resume (adapter reconnect) and
// Manager.RestoreActive (boot recovery for non-terminal roots).
//
// Phase 4: only roots use this path. Sub-agents do not survive
// process restart — settleDanglingSubagents (called below) appends
// session_terminated{reason="restart_died"} to every non-terminal
// child of this session and a synthetic subagent_result to this
// session's own events so the model, when it next materialises, sees
// each abandoned child as terminal with a clear instruction. The
// model decides whether to re-spawn — the runtime never auto-spawns.
//
// Returns ErrSessionClosed if the session has a session_terminated
// event already (caller handles as "session is gone, no resume").
func newSessionRestore(ctx context.Context, id string, parent *Session, deps *Deps) (*Session, error) {
	row, err := deps.Store.LoadSession(ctx, id)
	if err != nil {
		return nil, err
	}
	if hasTerminated(ctx, deps.Store, id) {
		return nil, ErrSessionClosed
	}

	// Settle any dangling sub-agents BEFORE bringing up the goroutine
	// so a) the synthetic subagent_result rows are persisted regardless
	// of whether lifecycle.Acquire / start succeed, and b) materialise
	// (called lazily on first inbound) sees a coherent parent.events.
	// Idempotent — second call (e.g. RestoreActive then Resume on the
	// same root) finds the rows already there and writes nothing.
	if _, err := settleDanglingSubagents(ctx, deps, id); err != nil {
		deps.Logger.Warn("session: restore settle dangling",
			"session", id, "err", err)
	}

	depth := depthFromRow(row)
	var parentCtx context.Context
	if parent != nil {
		parentCtx = parent.ctx
	} else {
		parentCtx = deps.RootCtx
	}
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	sessCtx, cancel := context.WithCancelCause(parentCtx)

	s := buildSessionShell(id, depth, parent, deps, sessCtx, cancel)
	s.openedAt = row.CreatedAt
	s.ownerID = row.OwnerID

	// Re-Acquire on the resume path: workspace dir is idempotent
	// (mkdir is a no-op when present), autoload re-binds the skill
	// catalogue, and per_session MCP providers re-spawn since they
	// don't survive a process exit. NOT idempotent under concurrent
	// resume of the same id — Lifecycle.AddSessionProvider rejects
	// duplicate (sessionID, name) registrations, so two adapters
	// racing Manager.Resume on the same id can leave one with a
	// half-built provider set. Manager.Resume's m.live double-check
	// at the end keeps the live tree clean (loser's session is
	// cancelled), but the brief window during construction is a
	// known follow-up — see review note L2.
	if deps.Lifecycle != nil {
		if err := deps.Lifecycle.Acquire(ctx, s); err != nil {
			cancel(nil)
			return nil, fmt.Errorf("session: re-acquire: %w", err)
		}
	}

	// Extension InitState — every extension allocates a fresh
	// per-session handle. For resumed sessions Recovery (lazy on the
	// first inbound frame inside materialise) replays events INTO
	// the handle that InitState seeded; the order keeps handle
	// pointers stable across the session's lifetime.
	for _, ext := range deps.Extensions {
		init, ok := ext.(extension.StateInitializer)
		if !ok {
			continue
		}
		if err := init.InitState(ctx, s); err != nil {
			deps.Logger.Warn("session: extension InitState (resume)",
				"session", id, "extension", ext.Name(), "err", err)
		}
	}

	// Emit a system_marker so adapters can render "session resumed"
	// in the transcript. Materialisation of in-memory projections
	// happens lazily on the first inbound Frame via s.materialise().
	marker := protocol.NewSystemMarker(id, deps.Agent.Participant(), "session_resumed",
		map[string]any{"prior_status": row.Status})
	if err := s.emit(ctx, marker); err != nil && !errors.Is(err, ErrSessionClosed) {
		deps.Logger.Warn("session: emit session_resumed marker", "session", id, "err", err)
	}

	return s, nil
}

// buildSessionShell constructs the *Session struct populated with the
// shared deps + tree links + ctx, but performs no IO. Used by both
// constructors so the field-init pattern stays single-sourced.
func buildSessionShell(id string, depth int, parent *Session, deps *Deps, sessCtx context.Context, cancel context.CancelCauseFunc) *Session {
	s := NewSession(id, deps.Agent, deps.Store, deps.Models, deps.Commands, deps.Codec, deps.Tools, deps.Logger, deps.Opts...)
	s.depth = depth
	s.deps = deps
	s.parent = parent
	s.children = make(map[string]*Session)
	s.ctx = sessCtx
	s.cancel = cancel
	return s
}

// Start launches the session's Run goroutine. The session's own
// internal ctx (s.ctx — derived from deps.RootCtx for roots and from
// parent.ctx for sub-agents) drives Run; the ctx parameter exists for
// future symmetry with idiomatic Start(ctx) APIs and for short-lived
// setup hooks. Today it is unused.
//
// Bookkeeping handled inline so callers carry no closures (Stage B
// retired the old `onExit` plumbing — parent.children deregister
// moves to handleSubagentResult; the only remaining hook was
// childWG.Done, now folded into the goroutine itself):
//
//   - deps.WG accounts for every session goroutine in the forest;
//     Manager.Stop waits on it.
//   - For sub-agents (s.parent != nil) the parent's childWG tracks
//     this child's goroutine so the parent's teardown can wait
//     specifically for ITS direct children to exit before running
//     its own lifecycle.Release / handleExit.
//
// Sessions built without deps (legacy NewSession test fixtures) do
// nothing — those callers manage goroutines themselves.
func (s *Session) Start(_ context.Context) {
	if s.deps == nil || s.deps.WG == nil {
		return
	}
	s.deps.WG.Add(1)
	if s.parent != nil {
		s.parent.childWG.Add(1)
	}
	go func() {
		defer s.deps.WG.Done()
		if s.parent != nil {
			defer s.parent.childWG.Done()
		}
		if err := s.Run(s.ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.deps.Logger.Warn("session loop exited", "session", s.id, "err", err)
		}
	}()
}

// hasTerminated returns true iff the session has at least one
// session_terminated event in its events. Cheap read-only walk used
// by Resume / Recover to gate "is this session gone?".
func hasTerminated(ctx context.Context, rs store.RuntimeStore, id string) bool {
	events, err := rs.ListEvents(ctx, id, store.ListEventsOpts{Limit: 1000})
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

// depthFromRow extracts metadata.depth from a SessionRow, handling
// both int and float64 (JSON unmarshal default) forms. Returns 0 if
// missing.
func depthFromRow(row store.SessionRow) int {
	if row.Metadata == nil {
		return 0
	}
	if d, ok := row.Metadata["depth"].(int); ok {
		return d
	}
	if df, ok := row.Metadata["depth"].(float64); ok {
		return int(df)
	}
	return 0
}

// appendTerminal appends a session_terminated event to the session's
// own events. Used on construction-failure paths (acquire_failed) and
// on goroutine exit when a terminationCause was attached. Best-effort:
// errors logged but not returned, since the caller is already
// returning an error of its own.
func (s *Session) appendTerminal(ctx context.Context, reason string) {
	terminal := protocol.NewSessionTerminated(s.id, s.deps.Agent.Participant(), protocol.SessionTerminatedPayload{
		Reason: reason,
	})
	row, summary, err := store.FrameToEventRow(terminal, s.deps.Agent.ID())
	if err != nil {
		s.deps.Logger.Warn("session: project terminal frame", "session", s.id, "err", err)
		return
	}
	if err := s.deps.Store.AppendEvent(ctx, row, summary); err != nil {
		s.deps.Logger.Warn("session: append terminal event", "session", s.id, "err", err)
	}
}
