package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

// spawn_mission is the root-only singular-spawn tool that root
// uses to delegate one user request to a mission session
// (phase 4.2.2 §4). Structurally identical to spawn_subagent with
// arity-1: root never fans out, it spawns one coordinator that
// then decomposes via spawn_wave. The split is deliberate — root
// having a singular spawn surface removes the "how many?"
// meta-decision that weak models repeatedly mishandle.
//
// γ adds two pieces on top of β:
//   - Catalogue validation against MissionDispatcher extensions
//     (only skills with metadata.hugen.mission.enabled:true accepted).
//   - on_mission_start hook: between Spawn and the first user-
//     message Submit the runtime fires the skill's pre-Run
//     scaffolding (plan.SystemSet + whiteboard.SystemInit + optional
//     first-message template override). Phase 4.2.2 §7.

const spawnMissionSchema = `{
  "type": "object",
  "properties": {
    "goal":   {"type": "string", "description": "What the mission must accomplish. Becomes the mission's first user message unless the dispatching skill's on_start overrides it."},
    "inputs": {"description": "Optional JSON the parent passes alongside the goal — schemas, anchors, prior context."},
    "skill":  {"type": "string", "description": "Skill that provides the mission coordinator pattern (e.g. analyst). Must be a mission-eligible skill (metadata.hugen.mission.enabled:true)."},
    "role":   {"type": "string", "description": "Role within the skill. Optional."}
  },
  "required": ["goal"]
}`

type spawnMissionInput struct {
	Goal   string `json:"goal"`
	Inputs any    `json:"inputs,omitempty"`
	Skill  string `json:"skill,omitempty"`
	Role   string `json:"role,omitempty"`
}

func (parent *Session) callSpawnMission(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if parent.IsClosed() {
		return toolErr("session_gone", "calling session has already terminated")
	}
	var in spawnMissionInput
	if err := json.Unmarshal(args, &in); err != nil {
		return toolErr("bad_request", fmt.Sprintf("invalid spawn_mission args: %v", err))
	}
	if strings.TrimSpace(in.Goal) == "" {
		return toolErr("bad_request", "goal is required")
	}

	// Resolve mission skill: explicit argument wins; otherwise fall
	// back to deps.DefaultMissionSkill.
	skillName := strings.TrimSpace(in.Skill)
	if skillName == "" && parent.deps != nil {
		skillName = parent.deps.DefaultMissionSkill
	}
	if mErr := validateMissionSkill(ctx, parent, skillName); mErr != nil {
		return mErr.toolError()
	}

	// Resolve on_start block — produces rendered plan body /
	// whiteboard flag / first-message override.
	block := resolveMissionStartBlock(ctx, parent, skillName, in.Goal, in.Inputs)

	// Effective task: override from on_start wins; otherwise the
	// caller-supplied goal.
	task := in.Goal
	if block != nil && block.FirstMessageOverride != "" {
		task = block.FirstMessageOverride
	}

	// Per-entry validation reuses spawn_subagent's helper machinery
	// (depth, skill/role describe). Mirroring the batch loop ensures
	// the LLM sees the same envelope shape from both tools.
	if rawErr := validateSpawnEntry(ctx, parent, skillName, in.Role, task); rawErr != nil {
		return rawErr, nil
	}

	spec := SpawnSpec{
		Skill:  skillName,
		Role:   in.Role,
		Task:   task,
		Inputs: in.Inputs,
	}
	child, err := parent.Spawn(ctx, spec)
	if err != nil {
		return toolErr("io", fmt.Sprintf("spawn_mission: spawn: %v", err))
	}

	// on_mission_start writes (plan + whiteboard) land on the
	// child's state AFTER Spawn (child goroutine running, idle
	// on inbox) and BEFORE Submit. The child's first turn picks
	// them up via state replay; emit ordering is preserved per
	// session via the internal event-log lock.
	if block != nil {
		applyMissionStartWrites(ctx, parent, child, block)
	}

	// Tier-default + per-role intent override on the spawned mission.
	parent.applyChildIntent(ctx, child, skillName, in.Role)

	// Deliver the first user message — the effective task (override
	// or original goal). Same pre/post IsClosed bracketing as
	// callSpawnSubagent so a child that died mid-spawn is logged
	// rather than silently dropped.
	first := protocol.NewUserMessage(child.ID(), parent.agent.Participant(), task)
	if !child.IsClosed() {
		<-child.Submit(ctx, first)
	}
	if child.IsClosed() {
		parent.logger.Warn("session: spawn_mission: child rejected initial task",
			"parent", parent.id, "child", child.ID())
	}

	return json.Marshal(spawnSubagentResult{
		SessionID: child.ID(),
		Depth:     child.depth,
	})
}

// validateSpawnEntry runs the per-entry validation common to
// spawn_subagent and spawn_mission: task non-empty, depth cap,
// skill/role describe. Returns a tool_error envelope on
// validation failure, nil on success. Phase 4.2.2 §4.
func validateSpawnEntry(ctx context.Context, parent *Session, skill, role, task string) json.RawMessage {
	if strings.TrimSpace(task) == "" {
		out, _ := toolErr("bad_request", "task is required")
		return out
	}
	maxDepth := DefaultMaxDepth
	if parent.deps != nil && parent.deps.MaxDepth > 0 {
		maxDepth = parent.deps.MaxDepth
	}
	if maxDepth > 0 && parent.depth+1 > maxDepth {
		out, _ := toolErr("depth_exceeded",
			fmt.Sprintf("parent depth %d + 1 exceeds runtime.max_depth %d",
				parent.depth, maxDepth))
		return out
	}
	if skill != "" {
		validation, err := describeSubagent(ctx, parent, skill, role)
		if err != nil {
			out, _ := toolErr("skill_not_found", err.Error())
			return out
		}
		switch validation {
		case extension.SubagentSkillFoundRoleMissing:
			out, _ := toolErr("role_not_found",
				fmt.Sprintf("role %q not declared in skill %q", role, skill))
			return out
		case extension.SubagentUnknown:
			if hasSubagentDescriber(parent) {
				out, _ := toolErr("skill_not_found",
					fmt.Sprintf("skill %q not found", skill))
				return out
			}
		}
	}
	return nil
}

// resolveMissionStartBlock walks deps.Extensions for the first
// MissionStartLookup and asks it to render the named skill's
// on_start. Returns nil when the skill has no on_start, no
// lookup is registered, or a lookup errored (logged + skipped).
func resolveMissionStartBlock(ctx context.Context, parent *Session, skill, goal string, inputs any) *extension.MissionStartBlock {
	if skill == "" || parent.deps == nil {
		return nil
	}
	for _, ext := range parent.deps.Extensions {
		lookup, ok := ext.(extension.MissionStartLookup)
		if !ok {
			continue
		}
		b, err := lookup.ResolveMissionStart(ctx, skill, goal, inputs)
		if err != nil {
			parent.logger.Warn("session: spawn_mission: ResolveMissionStart failed",
				"parent", parent.id, "skill", skill, "err", err)
			continue
		}
		if b != nil {
			return b
		}
	}
	return nil
}

// applyMissionStartWrites fires the plan + whiteboard system-
// principal write paths on the freshly-spawned mission's state
// per the resolved block. Errors are logged and swallowed — a
// misconfigured on_start template must not block the spawn.
func applyMissionStartWrites(ctx context.Context, parent *Session, child *Session, block *extension.MissionStartBlock) {
	if block == nil {
		return
	}
	if block.PlanText != "" {
		for _, ext := range parent.deps.Extensions {
			writer, ok := ext.(extension.PlanSystemWriter)
			if !ok {
				continue
			}
			if err := writer.SystemSet(ctx, child, block.PlanText, block.PlanCurrentStep); err != nil {
				parent.logger.Warn("session: spawn_mission: plan.SystemSet failed",
					"parent", parent.id, "child", child.id, "err", err)
			}
			break
		}
	}
	if block.WhiteboardInit {
		for _, ext := range parent.deps.Extensions {
			writer, ok := ext.(extension.WhiteboardSystemWriter)
			if !ok {
				continue
			}
			if err := writer.SystemInit(ctx, child); err != nil {
				parent.logger.Warn("session: spawn_mission: whiteboard.SystemInit failed",
					"parent", parent.id, "child", child.id, "err", err)
			}
			break
		}
	}
}

// missionValidationError carries the structured tool_error envelope
// validateMissionSkill produces. Wraps code + message so callers
// can either propagate via .toolError() or branch on .code in
// tests.
type missionValidationError struct {
	code    string
	message string
}

func (e *missionValidationError) toolError() (json.RawMessage, error) {
	return toolErr(e.code, e.message)
}

// validateMissionSkill checks the resolved skill against the
// MissionDispatcher catalogue. Returns nil when the skill is
// accepted, a structured envelope when it is rejected. Phase
// 4.2.2 §6.
func validateMissionSkill(ctx context.Context, parent *Session, skill string) *missionValidationError {
	if skill == "" {
		return &missionValidationError{
			code:    "no_mission_skill",
			message: "spawn_mission: no mission skill resolved — caller omitted `skill` AND no agent.default_mission_skill is configured. Install a skill with metadata.hugen.mission.enabled:true (e.g. analyst) or set the operator default.",
		}
	}
	if parent.deps == nil {
		return nil
	}
	var any bool
	for _, ext := range parent.deps.Extensions {
		disp, ok := ext.(extension.MissionDispatcher)
		if !ok {
			continue
		}
		any = true
		ok2, err := disp.MissionSkillExists(ctx, skill)
		if err != nil {
			parent.logger.Warn("session: spawn_mission: dispatcher error; treating as not-found",
				"parent", parent.id, "skill", skill, "err", err)
			continue
		}
		if ok2 {
			return nil
		}
	}
	if !any {
		return nil
	}
	return &missionValidationError{
		code:    "no_mission_skill",
		message: fmt.Sprintf("spawn_mission: skill %q is not registered as a mission dispatcher (no installed skill names %q with metadata.hugen.mission.enabled:true). See your system prompt's `## Available missions` block for the eligible options.", skill, skill),
	}
}
