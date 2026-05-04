package session

import (
	"context"
	"fmt"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// Spawn opens a sub-agent session as a child of s. The child row is
// written via newSession with session_type="subagent",
// parent_session_id = s.id, and metadata["depth"] = s.depth+1
// (immutable after create). Cancel-cascade flows through ctx because
// child.ctx is derived from s.ctx (ADR
// `phase-4-tree-ctx-routing.md` D7); parent termination automatically
// cancels every descendant.
//
// A subagent_started event is appended to s's events carrying the
// child id, role, task, depth, started_at, optional inputs, and the
// captured parent_whiteboard_active flag.
//
// The new child is registered in s.children (NOT in Manager.live —
// pivot 4 of the ADR makes m.live root-only). On goroutine exit the
// onExit callback removes the child from s.children.
//
// Errors:
//   - ErrDepthExceeded — s.depth+1 would exceed s.deps.maxDepth.
//   - whatever newSession returns on row-write or lifecycle.Acquire
//     failure (caller surfaces these to the spawning tool).
//
// Caller is responsible for permission / role.can_spawn checks (those
// land in commit 10 alongside the session:spawn_subagent tool).
func (s *Session) Spawn(ctx context.Context, spec SpawnSpec) (*Session, error) {
	if s.deps == nil {
		return nil, fmt.Errorf("session: spawn requires deps (constructed via newSession)")
	}
	if s.deps.maxDepth > 0 && s.depth+1 > s.deps.maxDepth {
		return nil, ErrDepthExceeded
	}
	childDepth := s.depth + 1
	childMeta := map[string]any{
		"depth":       childDepth,
		"spawn_role":  spec.Role,
		"spawn_skill": spec.Skill,
	}
	for k, v := range spec.Metadata {
		childMeta[k] = v
	}
	req := OpenRequest{
		OwnerID:            s.ownerID,
		ParentSessionID:    s.id,
		SpawnedFromEventID: spec.EventID,
		Metadata:           childMeta,
	}
	child, err := newSession(ctx, s, s.deps, req)
	if err != nil {
		return nil, fmt.Errorf("session: spawn: %w", err)
	}
	s.childMu.Lock()
	if s.children == nil {
		s.children = make(map[string]*Session)
	}
	s.children[child.id] = child
	s.childMu.Unlock()

	// Track the child on parent's childWG so parent's ctx.Done
	// teardown can wait specifically for ITS direct children to exit
	// before running its own lifecycle.Release / handleExit. The
	// shared deps.wg in Manager waits for every goroutine in the
	// forest; childWG narrows the wait to one tree level so the
	// "deepest leaves finish first, then their parent" ordering is
	// natural.
	s.childWG.Add(1)

	child.start(func() {
		s.childMu.Lock()
		if cur, ok := s.children[child.id]; ok && cur == child {
			delete(s.children, child.id)
		}
		s.childMu.Unlock()
		s.childWG.Done()
	})

	started := protocol.NewSubagentStarted(s.id, s.deps.agent.Participant(), protocol.SubagentStartedPayload{
		ChildSessionID:         child.ID(),
		Skill:                  spec.Skill,
		Role:                   spec.Role,
		Task:                   spec.Task,
		Depth:                  childDepth,
		StartedAt:              child.openedAt,
		Inputs:                 spec.Inputs,
		ParentWhiteboardActive: spec.ParentWhiteboardActive,
	})
	if err := s.emit(ctx, started); err != nil {
		s.deps.logger.Warn("session: emit subagent_started",
			"parent", s.id, "child", child.ID(), "err", err)
	}
	return child, nil
}

