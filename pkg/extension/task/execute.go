package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/skill"
)

// callExecuteTask runs the `task:execute_task(name, inputs)` generic
// runner — the discovery-then-run counterpart to the per-recipe
// `task:<recipe>` tools. It resolves the named task-eligible skill and
// runs it through the shared [Extension.RunRecipe] helper, anchored on
// the CALLER's own session.
//
// Anchoring differs from the synthetic `task:<recipe>` path on purpose
// (spec §5): `task:<recipe>` anchors on the caller's ROOT (results land
// in the user chat), whereas execute_task anchors on the caller's own
// session — a root caller still lands in chat, but a mission worker's
// sub-task nests under the worker and hands its result back like any
// tool. The mission-worker tool-approval policy is inherited for free:
// the mission ext's MaybeAutoApprove already walks the nested child's
// parent chain to the granting mission.
//
// Launch approval (§5.1, chat case) is NOT raised here yet — a task's
// internal tools still hit the normal per-tool approval gate. The
// two-mode launch modal lands in a follow-up step.
func (e *Extension) callExecuteTask(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	var in struct {
		Name   string          `json:"name"`
		Inputs json.RawMessage `json:"inputs,omitempty"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return toolErr("invalid_args",
				fmt.Sprintf("execute_task args is not valid JSON: %v", err)), nil
		}
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return toolErr("invalid_args", "execute_task requires a non-empty task name"), nil
	}
	if e.skills == nil {
		return toolErr("no_skill_manager", "skill manager not wired"), nil
	}

	sk, err := e.skills.Get(ctx, name)
	if err != nil {
		if errors.Is(err, skill.ErrSkillNotFound) {
			return toolErr("task_not_found",
				fmt.Sprintf("no task named %q — search with skill:catalog_list(task_eligible:true)", name)), nil
		}
		return nil, fmt.Errorf("execute_task: skill lookup %q: %w", name, err)
	}
	tb := sk.Manifest.Hugen.Task
	if !tb.Eligible {
		return toolErr("not_task_eligible",
			fmt.Sprintf("skill %q is not a runnable task (no task.eligible: true)", name)), nil
	}
	kind := tb.Kind
	if kind == "" {
		kind = skill.TaskKindWorker
	}
	if kind != skill.TaskKindWorker {
		return toolErr("kind_not_supported",
			fmt.Sprintf("task %q declares kind=%q; only kind=worker tasks run via execute_task today", name, kind)), nil
	}

	// Anchor = the CALLER's OWN session (not its root). host.Get on the
	// caller's id returns the live session it is dispatching from.
	state, ok := extension.SessionStateFromContext(ctx)
	if !ok || state == nil {
		return toolErr("no_session_state", "caller session state missing from context"), nil
	}
	host := e.sessionHost()
	if host == nil {
		return toolErr("no_session_host", "task ext is not bound to a session host"), nil
	}
	anchor, ok := host.Get(state.SessionID())
	if !ok {
		return toolErr("anchor_unavailable",
			fmt.Sprintf("caller session %s not live", state.SessionID())), nil
	}

	inputs, derr := decodeInputs(in.Inputs)
	if derr != nil {
		return toolErr("invalid_args", derr.Error()), nil
	}

	// Generate the launch goal from the task's goal_summary (skill_ref →
	// launch prose), falling back to a generic line for a task that
	// declared none.
	goal := strings.TrimSpace(tb.GoalSummary)
	if goal == "" {
		goal = fmt.Sprintf("Run the %s task once with the supplied inputs.", name)
	}
	spawnName := fmt.Sprintf("exec-%s-%d", name, e.spawnCounter.Add(1))

	res, rerr := e.RunRecipe(ctx, RunParams{
		Anchor:    anchor,
		Skill:     sk,
		Recipe:    name,
		SpawnName: spawnName,
		TaskBody:  goal,
		Inputs:    inputs,
		Tier:      skill.TierWorker,
		Metadata:  map[string]any{"task_recipe": name, "via": toolNameExecuteTask},
		// Model-driven launch → feeds the recipe-reuse bandit.
		CountAsUse: true,
	})
	if rerr != nil {
		if errors.Is(rerr, context.Canceled) || errors.Is(rerr, context.DeadlineExceeded) {
			return toolErr("call_cancelled", rerr.Error()), nil
		}
		return toolErr("spawn_failed", rerr.Error()), nil
	}
	return toolOK(name, spawnName, res.ChildID), nil
}
