package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// ErrDepthExceeded is returned by parent.Spawn when the new
// sub-agent's depth would exceed deps.maxDepth.
var ErrDepthExceeded = errors.New("session: max sub-agent depth exceeded")

// newSession creates a fresh Session (root or sub-agent), writes its
// initial sessions row, runs lifecycle.Acquire, and emits a
// SessionOpened frame. The returned Session is ready for s.start.
//
//   - parent == nil → root: ctx derived from deps.rootCtx, depth = 0,
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
func newSession(ctx context.Context, parent *Session, deps *sessionDeps, req OpenRequest) (*Session, error) {
	id := newSessionID()
	depth := 0
	sessionType := "root"
	var parentCtx context.Context
	if parent != nil {
		depth = parent.depth + 1
		sessionType = "subagent"
		parentCtx = parent.ctx
	} else {
		parentCtx = deps.rootCtx
	}
	if parentCtx == nil {
		// Defensive: should never happen post-pivot; deps.rootCtx is
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
	row := SessionRow{
		ID:                 id,
		AgentID:            deps.agent.ID(),
		OwnerID:            req.OwnerID,
		ParentSessionID:    req.ParentSessionID,
		SessionType:        sessionType,
		SpawnedFromEventID: req.SpawnedFromEventID,
		Status:             StatusActive,
		Metadata:           req.Metadata,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	if err := deps.store.OpenSession(ctx, row); err != nil {
		cancel(nil)
		return nil, fmt.Errorf("session: open row: %w", err)
	}
	s.openedAt = now
	s.ownerID = req.OwnerID

	// 3. Lifecycle.Acquire (workspace dir, autoload skills, per_session
	// MCPs). Failure here means the row exists but no goroutine —
	// mark the row terminal so restart-walker doesn't try to resume
	// it.
	if deps.lifecycle != nil {
		if err := deps.lifecycle.Acquire(ctx, id); err != nil {
			s.appendTerminal(ctx, "acquire_failed")
			cancel(nil)
			return nil, fmt.Errorf("session: acquire: %w", err)
		}
	}

	// 4. Emit SessionOpened so adapters / event log carry the live-cycle
	// marker. Only roots get a SessionOpened — sub-agents are signalled
	// to the parent's events via the SubagentStarted frame the caller
	// (parent.Spawn) emits next.
	if parent == nil {
		parts := req.Participants
		if len(parts) == 0 {
			parts = []protocol.ParticipantInfo{deps.agent.Participant()}
		}
		opened := protocol.NewSessionOpened(id, deps.agent.Participant(), parts)
		if err := s.emit(ctx, opened); err != nil && !errors.Is(err, ErrSessionClosed) {
			deps.logger.Warn("session: emit session_opened", "session", id, "err", err)
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
func newSessionRestore(ctx context.Context, id string, parent *Session, deps *sessionDeps) (*Session, error) {
	row, err := deps.store.LoadSession(ctx, id)
	if err != nil {
		return nil, err
	}
	if hasTerminated(ctx, deps.store, id) {
		return nil, ErrSessionClosed
	}

	// Settle any dangling sub-agents BEFORE bringing up the goroutine
	// so a) the synthetic subagent_result rows are persisted regardless
	// of whether lifecycle.Acquire / start succeed, and b) materialise
	// (called lazily on first inbound) sees a coherent parent.events.
	// Idempotent — second call (e.g. RestoreActive then Resume on the
	// same root) finds the rows already there and writes nothing.
	if _, err := settleDanglingSubagents(ctx, deps, id); err != nil {
		deps.logger.Warn("session: restore settle dangling",
			"session", id, "err", err)
	}

	depth := depthFromRow(row)
	var parentCtx context.Context
	if parent != nil {
		parentCtx = parent.ctx
	} else {
		parentCtx = deps.rootCtx
	}
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	sessCtx, cancel := context.WithCancelCause(parentCtx)

	s := buildSessionShell(id, depth, parent, deps, sessCtx, cancel)
	s.openedAt = row.CreatedAt
	s.ownerID = row.OwnerID

	// Idempotent re-Acquire: workspace dir already exists, autoload
	// re-binds, per_session MCPs respawn if needed.
	if deps.lifecycle != nil {
		if err := deps.lifecycle.Acquire(ctx, id); err != nil {
			cancel(nil)
			return nil, fmt.Errorf("session: re-acquire: %w", err)
		}
	}

	// Emit a system_marker so adapters can render "session resumed"
	// in the transcript. Materialisation of in-memory projections
	// happens lazily on the first inbound Frame via s.materialise().
	marker := protocol.NewSystemMarker(id, deps.agent.Participant(), "session_resumed",
		map[string]any{"prior_status": row.Status})
	if err := s.emit(ctx, marker); err != nil && !errors.Is(err, ErrSessionClosed) {
		deps.logger.Warn("session: emit session_resumed marker", "session", id, "err", err)
	}

	return s, nil
}

// buildSessionShell constructs the *Session struct populated with the
// shared deps + tree links + ctx, but performs no IO. Used by both
// constructors so the field-init pattern stays single-sourced.
func buildSessionShell(id string, depth int, parent *Session, deps *sessionDeps, sessCtx context.Context, cancel context.CancelCauseFunc) *Session {
	s := NewSession(id, deps.agent, deps.store, deps.models, deps.commands, deps.codec, deps.logger, deps.opts...)
	s.depth = depth
	s.deps = deps
	s.parent = parent
	s.children = make(map[string]*Session)
	s.ctx = sessCtx
	s.cancel = cancel
	return s
}

// start launches the session's Run goroutine. onExit (optional) is
// invoked after Run returns and after wg.Done — the caller uses it to
// remove the session from its parent's children map (or from
// Manager's m.live for roots).
func (s *Session) start(onExit func()) {
	if s.deps == nil || s.deps.wg == nil {
		// Legacy NewSession callers that haven't migrated to the
		// pivot constructors — they manage goroutines themselves.
		// This path is rare (test fixtures only).
		return
	}
	s.deps.wg.Add(1)
	go func() {
		defer s.deps.wg.Done()
		if onExit != nil {
			defer onExit()
		}
		if err := s.Run(s.ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.deps.logger.Warn("session loop exited", "session", s.id, "err", err)
		}
	}()
}

// hasTerminated returns true iff the session has at least one
// session_terminated event in its events. Cheap read-only walk used
// by Resume / Recover to gate "is this session gone?".
func hasTerminated(ctx context.Context, store RuntimeStore, id string) bool {
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

// depthFromRow extracts metadata.depth from a SessionRow, handling
// both int and float64 (JSON unmarshal default) forms. Returns 0 if
// missing.
func depthFromRow(row SessionRow) int {
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
	terminal := protocol.NewSessionTerminated(s.id, s.deps.agent.Participant(), protocol.SessionTerminatedPayload{
		Reason: reason,
	})
	row, summary, err := FrameToEventRow(terminal, s.deps.agent.ID())
	if err != nil {
		s.deps.logger.Warn("session: project terminal frame", "session", s.id, "err", err)
		return
	}
	if err := s.deps.store.AppendEvent(ctx, row, summary); err != nil {
		s.deps.logger.Warn("session: append terminal event", "session", s.id, "err", err)
	}
}

