package session

import (
	"context"
	"fmt"
	"maps"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
	skillpkg "github.com/hugr-lab/hugen/pkg/skill"
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
	// Phase 6.1d (tier-aware spawn): caller-supplied SpawnSpec.Tier
	// overrides the depth-derived default — recipes spawned by the
	// task ext at depth=1 carry worker semantics, not the
	// depth=1=mission default. Validate against the closed Tier*
	// constant set so a typo doesn't silently fall through to the
	// depth-derived fallback later. Empty stays empty here; newSession
	// applies skill.TierFromDepth(childDepth) when nothing was asked.
	switch spec.Tier {
	case "", skillpkg.TierRoot, skillpkg.TierMission, skillpkg.TierWorker:
	default:
		return nil, fmt.Errorf("session: spawn: invalid tier %q (want root|mission|worker)", spec.Tier)
	}

	// Phase 5.2 α: sanitise the model-supplied name and reserve a
	// collision-free slot among the parent's live children +
	// in-flight spawns. The reservation is released after we add
	// the child to s.children (or on error). Holding s.childMu
	// across resolve+reserve is fine — neither does I/O. Required-
	// ness of `name` is enforced at the JSON-schema layer of the
	// spawn tools; the sanitiser produces a fallback ("subagent")
	// for programmatic callers (tests) that leave it empty.
	s.childMu.Lock()
	if s.pendingNames == nil {
		s.pendingNames = make(map[string]struct{})
	}
	resolvedName := s.resolveChildNameLocked(spec.Name)
	s.pendingNames[resolvedName] = struct{}{}
	s.childMu.Unlock()

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
		// Phase 4.2.3 — record the spawn task as the child's
		// formal mission on the sessions row so observability
		// queries and prompt-time Block B "current mission" can
		// surface it without scanning events.
		Mission: spec.Task,
		Name:    resolvedName,
		// Phase 6.1d: forward the resolved tier override (already
		// validated above) into newSession. Empty here means
		// newSession falls back to skillpkg.TierFromDepth.
		Tier: spec.Tier,
	}
	child, err := newSession(ctx, s, s.deps, req)
	if err != nil {
		s.childMu.Lock()
		delete(s.pendingNames, resolvedName)
		s.childMu.Unlock()
		return nil, fmt.Errorf("session: spawn: %w", err)
	}
	// Phase 4.2.3 ε — duplicate spawn metadata on the in-memory
	// child handle so the close-turn resolver can pick
	// per-role on_close overrides without re-reading the row
	// from the store.
	child.spawnSkill = spec.Skill
	child.spawnRole = spec.Role
	child.mission = spec.Task
	// Mission-PDCA phase A — let external extensions (mission ext's
	// Plan Executor) request a non-default render mode without
	// reaching into the unexported `asyncSpawnMode` field. Mirrors
	// the existing in-package writes by tools_subagent_spawn_mission.go.
	if spec.RenderMode != "" {
		child.asyncSpawnMode = spec.RenderMode
	}
	s.logger.Debug("session: spawn: child constructed",
		"parent", s.id,
		"child", child.id,
		"name", resolvedName,
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
	delete(s.pendingNames, resolvedName)
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
	// abnormal-close finalizer.
	//
	// Phase 5.1c — track the pump goroutine on parent.childWG so
	// parent.drainOnTeardown waits for it before dispatchExtensionClosers
	// closes per-session state (e.g. liveview's channel that pump
	// pokes via ChildFrameObserver). Without this the pump can still
	// be draining buffered child frames into parent's liveview after
	// teardown closed liveview's channel — race + dropped frame.
	s.childWG.Go(func() { s.consumeChildOutbox(child) })

	started := protocol.NewSubagentStarted(s.id, s.deps.Agent.Participant(), protocol.SubagentStartedPayload{
		ChildSessionID: child.ID(),
		Name:           resolvedName,
		Skill:          spec.Skill,
		Role:           spec.Role,
		Task:           spec.Task,
		Depth:          childDepth,
		// Phase 6.1d: surface the resolved tier so observers (TUI
		// sidebar, liveview ChildMeta) render the override without
		// having to re-derive from depth.
		Tier:      child.tier,
		StartedAt: child.openedAt,
		Inputs:    spec.Inputs,
	})
	if err := s.emit(ctx, started); err != nil {
		s.deps.Logger.Warn("session: emit subagent_started",
			"parent", s.id, "child", child.ID(), "err", err)
	}
	// Apply per-role intent override (tier-default + skill-role hint)
	// and run every registered SubagentSpawnApplier (autoload_skills,
	// …) against the freshly-constructed child. Fires on every
	// Spawn() — session:spawn_mission tool, mission ext's executor
	// worker dispatch, tests. Phase H deleted the legacy
	// session:spawn_subagent LLM tool that used to own this wiring;
	// design-003 mission-PDCA brings it back here so worker roles
	// with `autoload_skills:` on their SubAgentRole actually receive
	// the bound skills before their first turn.
	s.applyChildIntent(ctx, child, spec.Skill, spec.Role)
	s.applyChildSpawnAppliers(ctx, child, spec.Skill, spec.Role)
	// Lifecycle: a parent with live children is not idle. The
	// in-flight-tool-call path already marked us active when the
	// turn started; this transition is the defensive backstop for
	// out-of-turn callers (test fixtures that Spawn directly
	// without driving a UserMessage). Guard drops the duplicate.
	s.markStatus(ctx, protocol.SessionStatusActive, "spawn")
	return child, nil
}

// SpawnChild implements [extension.SessionSpawner]. The capability
// lets external extensions (mission ext's Plan Executor) open a
// child session through a stable interface in pkg/extension —
// pkg/session never imports pkg/extension/mission so the spawner
// crosses the boundary structurally. Body is a thin translation
// to the in-package SpawnSpec; semantics are identical.
//
// Returning extension.SessionState (not *Session) keeps the
// public API in pkg/extension surface-clean.
func (s *Session) SpawnChild(ctx context.Context, spec extension.SpawnSpec) (extension.SessionState, error) {
	child, err := s.Spawn(ctx, SpawnSpec{
		Name:       spec.Name,
		Skill:      spec.Skill,
		Role:       spec.Role,
		Task:       spec.Task,
		Inputs:     spec.Inputs,
		Tier:       spec.Tier,
		RenderMode: spec.RenderMode,
	})
	if err != nil {
		return nil, err
	}
	return child, nil
}
