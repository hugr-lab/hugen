package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hugr-lab/hugen/pkg/extension"
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
		return e.dispatchWorker(ctx, short, args)
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
func (e *Extension) dispatchWorker(ctx context.Context, recipe string, args json.RawMessage) (json.RawMessage, error) {
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

	child, err := owner.Spawn(ctx, session.SpawnSpec{
		Name:  spawnName,
		Skill: recipe,
		Task:  fmt.Sprintf("Run the %s recipe once with the supplied inputs.", recipe),
		Inputs: inputs,
		Metadata: map[string]any{
			"task_recipe": recipe,
		},
	})
	if err != nil {
		return toolErr("spawn_failed", err.Error()), nil
	}

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
