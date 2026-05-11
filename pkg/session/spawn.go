package session

import (
	"context"
	"fmt"
	"maps"

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
// child id, role, task, depth, started_at, and optional inputs.
//
// The new child is registered in s.children (NOT in Manager.live —
// pivot 4 of the ADR makes m.live root-only). On goroutine exit the
// onExit callback removes the child from s.children.
//
// Errors:
//   - ErrDepthExceeded — s.depth+1 would exceed s.deps.MaxDepth.
//   - whatever newSession returns on row-write or lifecycle.Acquire
//     failure (caller surfaces these to the spawning tool).
//
// Caller is responsible for permission / role.can_spawn checks (those
// land in commit 10 alongside the session:spawn_subagent tool).
func (s *Session) Spawn(ctx context.Context, spec SpawnSpec) (*Session, error) {
	if s.deps == nil {
		return nil, fmt.Errorf("session: spawn requires deps (constructed via newSession)")
	}
	if s.deps.MaxDepth > 0 && s.depth+1 > s.deps.MaxDepth {
		return nil, ErrDepthExceeded
	}
	childDepth := s.depth + 1
	childMeta := map[string]any{
		"depth":       childDepth,
		"spawn_role":  spec.Role,
		"spawn_skill": spec.Skill,
	}
	maps.Copy(childMeta, spec.Metadata)
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
	s.logger.Debug("session: spawn: child constructed",
		"parent", s.id,
		"child", child.id,
		"parent_depth", s.depth,
		"child_depth", child.depth,
		"spawn_skill", spec.Skill,
		"spawn_role", spec.Role,
		"task_len", len(spec.Task))
	s.childMu.Lock()
	if s.children == nil {
		s.children = make(map[string]*Session)
	}
	s.children[child.id] = child
	s.childMu.Unlock()

	// childWG bookkeeping is now inside child.Start: it observes
	// s.parent (== current s) and Add/Done on s.childWG itself, so
	// the cascade / graceful-shutdown paths — which never go through
	// handleSubagentResult — still drain the WG. parent.children
	// deregistration lives in handleSubagentResult.
	child.Start(ctx)
	// Phase 4.1c: parent acts as adapter to child's outbox. The pump
	// goroutine reads child.Outbox(), projects cross-session-relevant
	// frames into parent's pipeline via parent.Submit, and drains the
	// rest. Without this, child's outbox back-fills its 32-buffer on
	// streaming chunks and emit blocks indefinitely. See
	// pkg/session/subagent_pump.go for the kind-level dispatch and
	// abnormal-close finalizer. Fire-and-forget — the range loop
	// exits naturally when child closes its outbox in Run's defer.
	go s.consumeChildOutbox(child)

	started := protocol.NewSubagentStarted(s.id, s.deps.Agent.Participant(), protocol.SubagentStartedPayload{
		ChildSessionID: child.ID(),
		Skill:          spec.Skill,
		Role:           spec.Role,
		Task:           spec.Task,
		Depth:          childDepth,
		StartedAt:      child.openedAt,
		Inputs:         spec.Inputs,
	})
	if err := s.emit(ctx, started); err != nil {
		s.deps.Logger.Warn("session: emit subagent_started",
			"parent", s.id, "child", child.ID(), "err", err)
	}
	// Lifecycle: a parent with live children is not idle. The
	// in-flight-tool-call path already marked us active when the
	// turn started; this transition is the defensive backstop for
	// out-of-turn callers (test fixtures that Spawn directly
	// without driving a UserMessage). Guard drops the duplicate.
	s.markStatus(ctx, protocol.SessionStatusActive, "spawn")
	return child, nil
}
