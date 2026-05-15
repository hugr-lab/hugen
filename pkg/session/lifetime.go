package session

import (
	"context"
	"time"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// Phase 5.2 ε — reasons recorded on session_terminated for runtime-
// hygiene auto-dismissals.
const (
	parkCeilingDropReason = "ceiling_drop"
	parkIdleTimeoutReason = "idle_timeout"
)

// resolveChildAutoclose walks the parent's deps.Extensions for the
// first [extension.AutocloseLookup] that returns a definitive answer
// for the child's (spawnSkill, spawnRole) pair. Returns true (the
// legacy auto-close path) when no extension speaks up — preserves
// status quo for sessions with no SkillManager wired (tests / no-
// skill deployments) and for children whose dispatching skill is
// no longer loaded on the parent.
//
// Phase 5.2 subagent-lifetime β.
func (s *Session) resolveChildAutoclose(ctx context.Context, child *Session) bool {
	if s.deps == nil || child == nil {
		return true
	}
	for _, ext := range s.deps.Extensions {
		l, ok := ext.(extension.AutocloseLookup)
		if !ok {
			continue
		}
		if val, found := l.ResolveAutoclose(ctx, s, child.spawnSkill, child.spawnRole); found {
			return val
		}
	}
	return true
}

// parkChild flips a child into the awaiting_dismissal lifecycle
// state instead of closing it. The child's Run loop stays alive
// (idle, waiting on its inbound channel) so a later
// session:notify_subagent can re-arm it OR a session:subagent_dismiss
// can tear it down via the existing SessionClose path.
//
// Called from the parent's [handleSubagentResult] when the child
// terminated normally (Reason == TerminationCompleted) AND the
// resolver chain produced autoclose=false. Any non-success reason
// (cancel, hard ceiling, abnormal close, panic, model error) keeps
// the legacy auto-close path: parking a broken / cancelled child
// would only waste the slot.
//
// Phase 5.2 subagent-lifetime β + ε:
//
//   - β set the lifecycle marker.
//   - ε layers two pieces of runtime hygiene around it:
//     (1) enforce Deps.MaxParkedChildrenPerRoot before marking — if
//         the root's subtree already holds the cap of parked children,
//         auto-dismiss the oldest with reason="ceiling_drop" first.
//     (2) arm an AfterFunc timer using Deps.ParkedIdleTimeout — on
//         expiry the runtime auto-dismisses with reason="idle_timeout".
//     notify_subagent re-arm clears the timer; an idempotent re-park
//     resets it.
func (s *Session) parkChild(ctx context.Context, child *Session) {
	if child == nil {
		return
	}
	s.enforceParkingCeiling(ctx, child)
	child.parkedAt.Store(time.Now().UnixNano())
	// markStatus is idempotent: a repeated park (e.g. observer
	// race) emits no second status frame.
	child.markStatus(ctx, protocol.SessionStatusAwaitingDismissal, "parked_on_result")
	s.armParkIdleTimer(child)
}

// childIsParked reports whether the given child is currently in
// the awaiting_dismissal lifecycle state. Used by routing /
// isQuiescent to skip parked children from "live work" counts.
func childIsParked(c *Session) bool {
	if c == nil {
		return false
	}
	return c.Status() == protocol.SessionStatusAwaitingDismissal
}

// ParkChildForTest is a test-only wrapper around parkChild. The
// runtime path runs from handleSubagentResult; tests that exercise
// parking-adjacent behaviour (restore, dismiss, ε ceiling)
// shortcut the result projection and drive parkChild directly.
func (s *Session) ParkChildForTest(ctx context.Context, child *Session) {
	s.parkChild(ctx, child)
}

// FindDescendant walks this session's subtree breadth-first and
// returns the first descendant whose ID matches. Used by tests
// (and future cross-tree lookups in the manager / TUI) to reach a
// specific child without the caller iterating Children()
// recursively. Returns (nil, false) when no match is found.
func (s *Session) FindDescendant(id string) (*Session, bool) {
	if s == nil || id == "" {
		return nil, false
	}
	queue := []*Session{s}
	for len(queue) > 0 {
		head := queue[0]
		queue = queue[1:]
		head.childMu.Lock()
		kids := make([]*Session, 0, len(head.children))
		for _, c := range head.children {
			if c == nil {
				continue
			}
			kids = append(kids, c)
		}
		head.childMu.Unlock()
		for _, c := range kids {
			if c.id == id {
				return c, true
			}
			queue = append(queue, c)
		}
	}
	return nil, false
}

// armParkIdleTimer schedules the per-child idle-timeout that auto-
// dismisses a parked child after Deps.ParkedIdleTimeout. Replaces
// any timer left over from a prior parking (idempotent re-park
// resets the clock). No-op when the deps bundle is absent or the
// timeout is zero (test wiring / explicit opt-out). Phase 5.2 ε.
func (parent *Session) armParkIdleTimer(child *Session) {
	if child == nil || parent.deps == nil {
		return
	}
	timeout := parent.deps.ParkedIdleTimeout
	if timeout <= 0 {
		return
	}
	child.parkTimerMu.Lock()
	defer child.parkTimerMu.Unlock()
	if child.parkTimer != nil {
		child.parkTimer.Stop()
	}
	child.parkTimer = time.AfterFunc(timeout, func() {
		parent.handleParkIdleTimeout(child)
	})
}

// cancelParkIdleTimer stops the child's idle timer (if any) and
// clears the slot. Safe to call when no timer is armed. Used by
// notify_subagent re-arm and explicit dismiss paths to prevent a
// stale fire from auto-dismissing a child the operator already
// addressed. Phase 5.2 ε.
func cancelParkIdleTimer(child *Session) {
	if child == nil {
		return
	}
	child.parkTimerMu.Lock()
	defer child.parkTimerMu.Unlock()
	if child.parkTimer != nil {
		child.parkTimer.Stop()
		child.parkTimer = nil
	}
}

// handleParkIdleTimeout is the AfterFunc body for ε's idle deadline.
// Runs on a runtime-owned goroutine — must NOT touch parent's
// per-Run state directly. Submits SessionClose to the child and
// spawns a goroutine that finalises the parent's children-map
// entry once the child's Run goroutine exits.
//
// Bails when:
//   - parent or child has already torn down (no-op),
//   - the child is no longer parked (a concurrent re-arm flipped it
//     active; the next park will arm a fresh timer if needed).
func (parent *Session) handleParkIdleTimeout(child *Session) {
	if parent == nil || parent.IsClosed() {
		return
	}
	if child == nil || child.IsClosed() {
		return
	}
	if !childIsParked(child) {
		return
	}
	if parent.logger != nil {
		parent.logger.Info("session: parked child idle timeout fired",
			"parent", parent.id, "child", child.id)
	}
	parent.dismissParkedChild(parent.idleDismissCtx(), child, parkIdleTimeoutReason)
}

// enforceParkingCeiling implements the Deps.MaxParkedChildrenPerRoot
// cap. Counts every parked descendant of the root, and while the
// count meets-or-exceeds the cap auto-dismisses the oldest parked
// child (smallest parkedAt) with reason="ceiling_drop" first. The
// newcomer (the child about to be parked) is excluded from the
// candidate set even though it isn't yet marked parked — defence
// against subtle reorderings if a future refactor flips the call
// order. Phase 5.2 ε.
func (s *Session) enforceParkingCeiling(ctx context.Context, newcomer *Session) {
	if s.deps == nil {
		return
	}
	cap := s.deps.MaxParkedChildrenPerRoot
	if cap <= 0 {
		return
	}
	root := s
	for root.parent != nil {
		root = root.parent
	}
	for {
		parked := collectParkedSubtree(root, newcomer)
		if len(parked) < cap {
			return
		}
		oldest := oldestParked(parked)
		if oldest == nil {
			return
		}
		// Address the parent that owns this parked child, not the
		// outer s (parking a mission's child must evict the
		// mission's parent's oldest parked sibling correctly).
		owner := oldest.parent
		if owner == nil {
			// Defensive — a parked root has no semantic and the
			// ceiling cannot evict it.
			return
		}
		if s.logger != nil {
			s.logger.Info("session: parked child evicted by ceiling",
				"root", root.id, "evicted", oldest.id, "cap", cap)
		}
		owner.dismissParkedChild(ctx, oldest, parkCeilingDropReason)
	}
}

// collectParkedSubtree walks the session tree rooted at root and
// returns every descendant currently in awaiting_dismissal,
// excluding skip (the newcomer about to be parked). Single-pass
// DFS; each level locks its own childMu briefly. Phase 5.2 ε.
func collectParkedSubtree(root, skip *Session) []*Session {
	var out []*Session
	var visit func(*Session)
	visit = func(n *Session) {
		if n == nil {
			return
		}
		n.childMu.Lock()
		// Copy slice references so we can release the lock before
		// recursing — childMu must not be held across child calls
		// that may themselves take child locks.
		kids := make([]*Session, 0, len(n.children))
		for _, c := range n.children {
			kids = append(kids, c)
		}
		n.childMu.Unlock()
		for _, c := range kids {
			if c == nil || c == skip {
				continue
			}
			if childIsParked(c) {
				out = append(out, c)
			}
			visit(c)
		}
	}
	visit(root)
	return out
}

// oldestParked returns the parked session with the smallest
// parkedAt. nil for an empty slice. Stable enough for the ceiling
// path — ties on parkedAt are vanishingly unlikely in practice.
// Phase 5.2 ε.
func oldestParked(parked []*Session) *Session {
	var best *Session
	var bestAt int64
	for _, c := range parked {
		at := c.parkedAt.Load()
		if best == nil || at < bestAt {
			best = c
			bestAt = at
		}
	}
	return best
}

// dismissParkedChild is the shared post-parking close path. Used by
// the runtime auto-dismiss paths (ε idle timeout, ε ceiling drop)
// and reused by the explicit session:subagent_dismiss tool to
// guarantee the children-map cleanup runs even though the pump's
// st.projected gate suppresses a second SubagentResult after a
// child's initial completion.
//
// Concurrency model: SessionClose Submit returns quickly; the
// child's Run goroutine processes the close on its own timeline.
// We spawn a finaliser goroutine that waits on child.Done() and
// then deletes the entry from parent.children + sweeps inquiry
// routes. Caller may continue without waiting; the parent's next
// routeInbound cycle re-evaluates quiescence.
//
// Idempotent: a duplicate dismiss for an already-closed child is
// caught by the IsClosed early exit; a duplicate for a still-live
// but already-dismissing child is harmless (Submit drops onto a
// soon-to-close inbox; the finaliser goroutine still runs the
// children-map cleanup, which is itself idempotent via the
// conditional delete). Phase 5.2 ε.
func (parent *Session) dismissParkedChild(ctx context.Context, child *Session, reason string) {
	if parent == nil || child == nil {
		return
	}
	if child.IsClosed() {
		parent.deregisterDismissedChild(child.id, child)
		return
	}
	cancelParkIdleTimer(child)
	closeFrame := protocol.NewSessionClose(child.id, parent.agent.Participant(), reason)
	_ = child.Submit(ctx, closeFrame)
	go func() {
		select {
		case <-child.Done():
		case <-parent.rootDone():
			return
		}
		parent.deregisterDismissedChild(child.id, child)
	}()
}

// deregisterDismissedChild removes a closed child from parent.children
// (conditional on identity to avoid clobbering a fresh entry that
// reused the id post-resume) and sweeps the inquiry-response route
// table. Safe to call from any goroutine — only touches mutex-
// guarded state. Phase 5.2 ε.
//
// The matching expected-children registration lives in handleSubagentResult;
// auto-dismissal duplicates this finalisation because the pump's
// st.projected gate prevents a second SubagentResult after the
// initial completion projection.
func (parent *Session) deregisterDismissedChild(childID string, identity *Session) {
	if parent == nil || childID == "" {
		return
	}
	parent.childMu.Lock()
	if cur, ok := parent.children[childID]; ok && (identity == nil || cur == identity) {
		delete(parent.children, childID)
	}
	parent.childMu.Unlock()
	parent.sweepResponseRoutesForChild(childID)
}

// idleDismissCtx returns the parent's root-scoped ctx for auto-
// dismiss Submit calls fired from outside the Run loop (idle
// timer, ceiling enforcement goroutine). Falls back to a fresh
// Background when deps is missing (test wiring). The ctx is only
// observed for the Submit (non-blocking after channel push); the
// child's teardown runs on its own runCtx independently.
func (s *Session) idleDismissCtx() context.Context {
	if s.deps != nil && s.deps.RootCtx != nil {
		return s.deps.RootCtx
	}
	return context.Background()
}

// rootDone returns a channel that closes when the root context is
// cancelled. Used by dismiss finaliser goroutines to bail on
// runtime shutdown without leaking. Returns a never-closing channel
// when deps is absent (test wiring) so the goroutine waits solely
// on child.Done().
func (s *Session) rootDone() <-chan struct{} {
	if s.deps != nil && s.deps.RootCtx != nil {
		return s.deps.RootCtx.Done()
	}
	return make(chan struct{})
}
