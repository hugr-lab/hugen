package task

import (
	"context"
	"fmt"

	"github.com/hugr-lab/hugen/pkg/extension"
	skillext "github.com/hugr-lab/hugen/pkg/extension/skill"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session"
	"github.com/hugr-lab/hugen/pkg/skill"
)

// RunParams configures a single recipe launch through [Extension.RunRecipe]
// — the one execute path behind every task surface: the synthetic
// `task:<recipe>` tool (ad-hoc), `execute_task` (named, Phase B47 step 2),
// and the cron spawn-fire. The caller resolves the anchor session +
// recipe skill and mints the spawn name itself (anchoring policy, drift
// checks, and the per-spawn applier rendezvous are caller concerns); the
// helper owns the mechanics: spawn → scope skill surface → pre-load →
// kick → wait.
type RunParams struct {
	// Anchor is the live session the recipe child spawns under, becoming
	// its subagent — so Anchor's history receives the SubagentResult
	// projection the model reads. Anchoring policy differs per caller:
	// task:<recipe> anchors on the caller's root, cron on the schedule
	// owner root, execute_task on the caller's own session. Required.
	Anchor *session.Session

	// Skill is the resolved recipe skill. Its manifest drives the
	// AllowedSkills whitelist scoping the child's skill surface and the
	// requires_skills closure pre-loaded before the first turn.
	Skill skill.Skill

	// Recipe is the skill name (== Skill.Manifest.Name). Used for the
	// SpawnSpec.Skill autoload, the explicit skill:load, and audit logs.
	Recipe string

	// SpawnName is the pre-generated unique child name. Required:
	// callers that rendezvous a per-spawn applier (cron's FireContext
	// stash) MUST generate + stash under the same name, so the helper
	// never mints its own — that would desync the stash key.
	SpawnName string

	// TaskBody is the kick message the child's first turn fires on.
	// task:<recipe> passes a fixed "run the recipe once" line; cron
	// passes the rendered goal template.
	TaskBody string

	// Inputs is the structured input value threaded into SpawnSpec and
	// the `[Inputs]` block of the first user message. nil for no-input
	// recipes.
	Inputs any

	// Tier overrides the spawn tier (one of skill.Tier{Root,Mission,
	// Worker}, "" for default depth-based resolution). task:<recipe>
	// pins TierWorker so a depth-1 recipe child reads as a leaf
	// executor, not a coordinator; cron leaves it empty.
	Tier string

	// Metadata is merged onto the child row for liveview / audit
	// grouping by source (task_recipe, cron_task_id, …).
	Metadata map[string]any

	// CountAsUse marks a model-driven launch (chat / mission worker) so
	// the run records a bandit `use` event on clean completion. Cleared
	// for headless cron fires (no model decided to run it). The bandit
	// recorder is wired in the advertise step; today the flag gates a
	// debug trace at the single completion choke point.
	CountAsUse bool

	// RaiseLaunchApproval requests the §5.1 launch modal on the anchor
	// before spawning: the user approves LAUNCHING the (standardized)
	// task, optionally auto-approving its internal tools. Set by the
	// chat tool callers (task:<recipe> / execute_task from a root chat);
	// cleared for headless cron and for mission-worker callers (whose
	// tool policy is inherited via the mission ext's MaybeAutoApprove).
	// A reject returns RunResult{Rejected:true} without spawning.
	RaiseLaunchApproval bool

	// AutoApproveTools pre-stamps the spawned worker with a blanket
	// tool auto-approve (the §5.1 "approve with tools" effect) WITHOUT a
	// modal. Set by the headless cron fire: a scheduled task is
	// pre-approved by the user scheduling it, and there is no operator
	// at fire time, so its tools must auto-approve. Chat callers leave
	// this false and let RaiseLaunchApproval's modal decide — when a
	// modal is raised its result is authoritative (overrides this).
	AutoApproveTools bool

	// RenderMode tags the spawned worker's terminal SubagentResult.
	// Headless cron sets protocol.SubagentRenderAsyncNotify so the
	// result, arriving at an IDLE owner root, arms the auto-summary turn
	// and the model proactively surfaces it to the user (without it the
	// result lands in history but nothing kicks a turn to show it). Chat
	// callers leave it "": their worker completes mid-turn and the
	// result is folded into the active turn, so the root surfaces it
	// inline already.
	RenderMode string
}

// RunResult carries the spawned child's id for the caller's projection
// (toolOK ack / task_log completed row). The recipe's textual result
// lands in the anchor's history via the standard SubagentResult
// projection — this is just the cross-reference id.
type RunResult struct {
	ChildID string

	// Rejected is true when a §5.1 launch modal was raised and the user
	// declined — no child was spawned. ChildID is empty. RefineText
	// carries any free-text the user added on the reject.
	Rejected   bool
	RefineText string
}

// RunRecipe is the shared execute path: spawn a recipe child under
// p.Anchor, scope its skill surface to the manifest whitelist, pre-load
// the recipe body + its requires_skills closure, kick the first turn,
// and block until the child terminates (or ctx ends).
//
// Error contract — the caller maps these onto its own surface:
//
//   - spawn failure → non-nil error, RunResult{} (no child). NOT a
//     context error.
//   - ctx cancelled during the wait → (RunResult{ChildID}, ctx.Err()).
//     The child spawned; the caller distinguishes a timeout from a
//     spawn failure via errors.Is(err, context.Canceled/DeadlineExceeded).
//   - clean termination → (RunResult{ChildID}, nil).
func (e *Extension) RunRecipe(ctx context.Context, p RunParams) (RunResult, error) {
	if p.Anchor == nil {
		return RunResult{}, fmt.Errorf("task: RunRecipe: nil anchor session")
	}
	if p.SpawnName == "" {
		return RunResult{}, fmt.Errorf("task: RunRecipe: empty spawn name")
	}

	// §5.1 launch approval (chat callers only) — raised BEFORE the spawn
	// so a reject costs nothing. approve-with-tools flows into the
	// per-worker auto-approve stamp below. Headless cron pre-sets
	// AutoApproveTools directly (no operator to answer a modal).
	autoApproveTools := p.AutoApproveTools
	if p.RaiseLaunchApproval {
		decision, err := e.raiseLaunchApproval(ctx, p.Anchor, p.Recipe, p.Skill)
		if err != nil {
			return RunResult{}, err
		}
		if !decision.approved {
			return RunResult{Rejected: true, RefineText: decision.refine}, nil
		}
		autoApproveTools = decision.autoApproveTools
	}

	child, err := p.Anchor.Spawn(ctx, session.SpawnSpec{
		Name:       p.SpawnName,
		Skill:      p.Recipe,
		Task:       p.TaskBody,
		Inputs:     p.Inputs,
		Tier:       p.Tier,
		Metadata:   p.Metadata,
		RenderMode: p.RenderMode,
	})
	if err != nil {
		return RunResult{}, fmt.Errorf("task: spawn recipe %q: %w", p.Recipe, err)
	}

	// Scope the recipe child's `skill:load` + `## Available skills`
	// catalogue to the manifest-declared AllowedSkills whitelist. An
	// empty list locks the surface to whatever the spawner pre-loaded
	// (universal `_system`/`_worker` + RequiresSkills); a populated list
	// adds reachable-via-load entries on top. Pass `[]string` directly
	// so the skill ext's typed switch hits the fast path.
	allowList := append([]string(nil), p.Skill.Manifest.Hugen.AllowedSkills...)
	child.SetValue(skillext.SessionAllowedSkillsKey, allowList)

	// §5.1 approve-with-tools — stamp the worker so its standardized
	// tools auto-approve via this ext's ToolApprovalPolicy. Set BEFORE
	// the kick so the worker's first tool call is already covered. No
	// stamp on plain approve (each tool prompts) or when no modal ran.
	if autoApproveTools {
		child.SetValue(taskAutoApproveToolsKey, true)
	}

	// Pre-load the recipe body (so its steps land in the system prompt)
	// plus its requires_skills closure (no skill:load round-trip before
	// the first step). SkillManager.Load walks the closure itself, so
	// the per-dep loop is belt-and-braces — it surfaces individual
	// failures in the log instead of hiding them in one aggregate. A
	// per-skill failure logs + skips: one missing dependency must not
	// deny the recipe its baseline surface.
	e.preloadRecipeSkills(ctx, child, p.Skill, p.Recipe)

	// Kick the first turn. Without this the child sits idle after
	// autoload — Session.Spawn runs appliers but never injects an
	// inbound user frame, so the model loop has nothing to fire on.
	// This is the line whose ABSENCE made cron spawn-fires dead (B46);
	// routing cron through RunRecipe is what closes it.
	first := protocol.NewUserMessage(child.ID(), e.agentParticipant(),
		buildFirstMessage(p.TaskBody, p.Inputs))
	_ = child.Submit(ctx, first)

	// Wait for the subagent to terminate. close_turn ⇒ SessionClose ⇒
	// teardown emits SubagentResult into the anchor's pipeline. Per-call
	// cancellation flows through ctx (tool-dispatch cap / runner per-fire
	// timeout / stuck-detector).
	select {
	case <-child.Done():
		if p.CountAsUse {
			// Step 5 (advertise) swaps this debug trace for the bandit
			// `use` event — RunRecipe is the single choke point where a
			// model-driven recipe run completes.
			e.logger.Debug("task: recipe run counted as use",
				"recipe", p.Recipe, "child", child.ID())
		}
		return RunResult{ChildID: child.ID()}, nil
	case <-ctx.Done():
		return RunResult{ChildID: child.ID()}, ctx.Err()
	}
}

// preloadRecipeSkills loads the recipe body + its requires_skills
// closure into the freshly-spawned child's skill state. No-op when the
// child carries no skill state (pathological wiring). Per-skill failures
// log at warn + skip — see RunRecipe's pre-load comment for the rationale.
func (e *Extension) preloadRecipeSkills(ctx context.Context, child extension.SessionState, sk skill.Skill, recipe string) {
	skillState := skillext.FromState(child)
	if skillState == nil {
		return
	}
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
