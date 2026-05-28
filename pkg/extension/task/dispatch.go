package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hugr-lab/hugen/pkg/extension"
	skillext "github.com/hugr-lab/hugen/pkg/extension/skill"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session"
	"github.com/hugr-lab/hugen/pkg/skill"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// Call implements [tool.ToolProvider]. Routes synthetic
// `task:<recipe-name>` calls into a fresh subagent spawn against the
// named recipe skill. The recipe's manifest decides the spawn
// shape:
//
//   - `task.kind: worker` (default) — spawns a leaf subagent under
//     the caller's root session, waits for its terminal handoff via
//     mission:finish, and returns the handoff body as tool_result.
//   - `task.kind: mission` — not yet wired in this MVP; returns an
//     explicit "not yet supported" error so a future phase can layer
//     in the mission-shape spawn without changing this surface.
//
// Failure modes carry a structured `toolError` JSON body so the
// model can react with the appropriate retry path (fix args, choose
// a different recipe, escalate to user) instead of silently looping.
func (e *Extension) Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	// ToolManager.Dispatch passes the short name (after `toolField`
	// strips the `task:` prefix) — but we accept either form
	// defensively for tests / future direct callers.
	short := stripProviderPrefix(name)
	if short == "" {
		return nil, fmt.Errorf("%w: %s", tool.ErrUnknownTool, name)
	}
	if e.skills == nil {
		return toolErr("no_skill_manager", "skill manager not wired"), nil
	}
	sk, err := e.skills.Get(ctx, short)
	if err != nil {
		if errors.Is(err, skill.ErrSkillNotFound) {
			return toolErr("recipe_not_found",
				fmt.Sprintf("no skill named %q in the manifest catalogue", short)), nil
		}
		return nil, fmt.Errorf("task: skill lookup %q: %w", short, err)
	}
	tb := sk.Manifest.Hugen.Task
	if !tb.Eligible {
		return toolErr("not_task_eligible",
			fmt.Sprintf("skill %q does not declare task.eligible: true", short)), nil
	}

	kind := tb.Kind
	if kind == "" {
		kind = skill.TaskKindWorker
	}

	switch kind {
	case skill.TaskKindWorker:
		return e.dispatchWorker(ctx, sk, short, args)
	case skill.TaskKindMission:
		return toolErr("kind_not_supported",
			fmt.Sprintf("task.kind=%q is not yet wired in this Phase 6.1d MVP; recipe %q must declare kind=worker for ad-hoc execution", kind, short)), nil
	default:
		return toolErr("unknown_kind",
			fmt.Sprintf("task.kind=%q is not a recognised value (worker|mission)", kind)), nil
	}
}

// dispatchWorker is the kind=worker branch: spawn a leaf subagent
// under the caller's root with the recipe skill + caller's args,
// then block until the subagent terminates and project its terminal
// handoff body back as the tool_result.
//
// The spawn anchors on the caller's root session — recipes are a
// user-facing concern so their results land in the chat history the
// user is watching. Workers spawned by a mission's executor pump
// (synthesis, planner waves, etc.) shouldn't reach this surface;
// task:* is root-tier-only by allow-set design.
func (e *Extension) dispatchWorker(ctx context.Context, sk skill.Skill, recipe string, args json.RawMessage) (json.RawMessage, error) {
	state, ok := extension.SessionStateFromContext(ctx)
	if !ok || state == nil {
		return toolErr("no_session_state", "caller session state missing from context"), nil
	}
	host := e.sessionHost()
	if host == nil {
		return toolErr("no_session_host", "task ext is not bound to a session host"), nil
	}
	root := rootOf(state)
	owner, ok := host.Get(root.SessionID())
	if !ok {
		return toolErr("owner_unavailable",
			fmt.Sprintf("owner session %s not live", root.SessionID())), nil
	}

	// Materialise args as a structured value for SpawnSpec.Inputs.
	// Empty args are common (no-input recipe); nil works downstream.
	var inputs any
	if len(args) > 0 && string(args) != "null" {
		if err := json.Unmarshal(args, &inputs); err != nil {
			return toolErr("invalid_args",
				fmt.Sprintf("args is not valid JSON: %v", err)), nil
		}
	}

	// Spawn name uniqueness within the parent — pkg/session.Spawn
	// sanitises + collision-suffixes, but generating a token here
	// keeps the per-call audit name predictable.
	spawnName := fmt.Sprintf("task-%s-%d", recipe, e.spawnCounter.Next())

	taskBody := fmt.Sprintf("Run the %s recipe once with the supplied inputs.", recipe)
	child, err := owner.Spawn(ctx, session.SpawnSpec{
		Name:   spawnName,
		Skill:  recipe,
		Task:   taskBody,
		Inputs: inputs,
		// Phase 6.1d: pin the recipe child to worker tier so its
		// structural depth=1 (child of root) doesn't get mission
		// semantics. Recipes are leaf executors, not coordinators —
		// skill autoload then matches the recipe manifest's
		// `tier_compatibility: [worker]`; constitution + compactor
		// overlays + routing intent all see worker.
		Tier: skill.TierWorker,
		Metadata: map[string]any{
			"task_recipe": recipe,
		},
	})
	if err != nil {
		return toolErr("spawn_failed", err.Error()), nil
	}

	// Phase 6.1d — scope the recipe child's `skill:load` and
	// `## Available skills` catalogue to the manifest-declared
	// AllowedSkills whitelist. The skill extension reads this Value
	// (key SessionAllowedSkillsKey) at callLoad + catalogue render.
	// An empty list locks the surface to whatever was pre-loaded by
	// the spawner (universal `_system`/`_worker` + RequiresSkills);
	// a populated list adds reachable-via-skill-load entries on top
	// of that. Mission ext spawns do NOT set this — wave-workers
	// keep full dynamic-load flexibility. Pass `[]string` directly
	// so handlers.go's typed switch hits the fast path without
	// reaching the []any reflection branch.
	allowList := append([]string(nil), sk.Manifest.Hugen.AllowedSkills...)
	child.SetValue(skillext.SessionAllowedSkillsKey, allowList)

	// Phase 6.1d — load the recipe skill itself (so its body lands
	// in the system prompt and the LLM actually sees the steps it
	// must execute) plus its `requires_skills` declared dependencies
	// (eager pre-load, no `skill:load` round-trip needed before the
	// recipe's first step). SkillManager.Load walks the closure via
	// `requires_skills` automatically, so the explicit per-dep loop
	// is belt-and-braces — it surfaces individual failures in the
	// log without the closure walk hiding them inside one aggregate
	// error. Per-skill failures log + skip — one missing dependency
	// must not deny the recipe its baseline surface; the recipe
	// body limps along on partial deps and the result handoff
	// surfaces the gap.
	if skillState := skillext.FromState(child); skillState != nil {
		if loadErr := skillState.Load(ctx, recipe); loadErr != nil {
			e.logger.Warn("task: load recipe skill failed",
				"recipe", recipe, "err", loadErr)
		}
		for _, dep := range sk.Manifest.Hugen.RequiresSkills {
			if loadErr := skillState.Load(ctx, dep); loadErr != nil {
				e.logger.Warn("task: pre-load requires_skills failed",
					"recipe", recipe, "dep", dep, "err", loadErr)
			}
		}
	}

	// Phase 6.1d: deliver the first UserMessage so the recipe child
	// actually starts a turn. Without this the child sits idle after
	// autoload — Session.Spawn opens the session and runs appliers
	// but never injects an inbound user frame; the LLM loop has
	// nothing to fire on. Mirrors the mission ext's planner.go
	// spawner callback (which builds the wave-worker first message
	// the same way).
	first := protocol.NewUserMessage(child.ID(), e.agentParticipant(),
		buildFirstMessage(taskBody, inputs))
	_ = child.Submit(ctx, first)

	// Wait for the subagent to terminate. Per-call cancellation
	// flows through ctx — the tool dispatcher caps tool runs with
	// its own timeout, and the runtime's stuck-detector eventually
	// fires SessionClose on a runaway recipe.
	select {
	case <-child.Done():
	case <-ctx.Done():
		return toolErr("call_cancelled", ctx.Err().Error()), nil
	}

	// Recipe's terminal text + reason land in the parent's chat
	// history via the standard SubagentResult projection — the LLM
	// reads them there. The tool_result returned here is the
	// completion ack, naming the child session_id for cross-
	// referencing.
	return toolOK(recipe, spawnName, child.ID()), nil
}

// agentParticipant builds the participant the task extension stamps
// onto the synthetic UserMessage it injects into a recipe child to
// drive the first turn. Mirrors the mission ext's agentParticipant
// helper but lives here to keep task ext free of cross-extension
// imports.
func (e *Extension) agentParticipant() protocol.ParticipantInfo {
	return protocol.ParticipantInfo{
		ID:   e.agentID,
		Kind: protocol.ParticipantAgent,
		Name: "hugen",
	}
}

// buildFirstMessage renders the recipe child's first user message
// body. Trivial inputs (nil / empty map / empty array) leave the
// task description as-is; otherwise the inputs JSON is prepended
// under an `[Inputs]` block so the LLM sees concrete values for
// every key its recipe body references. Mirrors mission ext's
// `buildWorkerFirstMessage` (Phase A) but kept local to avoid a
// cross-extension dependency.
func buildFirstMessage(task string, inputs any) string {
	if inputs == nil {
		return task
	}
	raw, err := json.MarshalIndent(inputs, "", "  ")
	if err != nil {
		return task
	}
	trimmed := strings.TrimSpace(string(raw))
	switch trimmed {
	case "", "null", "{}", "[]", `""`:
		return task
	}
	return "[Inputs]\n" + trimmed + "\n\n[Task]\n" + task
}

// rootOf walks the parent chain until it finds the depth-0 ancestor.
// Recipes anchor on root because the chat the user reads belongs to
// the root session — projecting recipe results elsewhere would
// orphan them from the conversation. Mirrors scheduler ext's
// rootOf helper.
func rootOf(state extension.SessionState) extension.SessionState {
	for {
		if state == nil {
			return nil
		}
		parent, has := state.Parent()
		if !has || parent == nil {
			return state
		}
		state = parent
	}
}

// toolError is the structured failure shape every Call branch
// returns when it short-circuits with a recoverable error. Matches
// the scheduler ext's toolError convention so the model sees one
// consistent error shape across both extensions.
type toolError struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

type toolErrorResponse struct {
	OK    bool      `json:"ok"`
	Error toolError `json:"error"`
}

func toolErr(code, msg string) json.RawMessage {
	resp := toolErrorResponse{
		OK: false,
		Error: toolError{
			Code:    code,
			Message: msg,
		},
	}
	raw, _ := json.Marshal(resp)
	return raw
}

// toolOKResponse is the success ack the model sees once the recipe
// has terminated. The recipe's actual textual result lands in the
// parent's chat history via the standard SubagentResult projection
// — this payload is just a cross-reference so the model can correlate
// the ack with the projected result in the same turn.
type toolOKResponse struct {
	OK             bool   `json:"ok"`
	Recipe         string `json:"recipe"`
	SpawnName      string `json:"spawn_name"`
	ChildSessionID string `json:"child_session_id"`
}

func toolOK(recipe, spawnName, childID string) json.RawMessage {
	resp := toolOKResponse{
		OK:             true,
		Recipe:         recipe,
		SpawnName:      spawnName,
		ChildSessionID: childID,
	}
	raw, _ := json.Marshal(resp)
	return raw
}
