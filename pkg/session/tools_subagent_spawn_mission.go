package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

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
    "goal":       {"type": "string", "description": "What the mission must accomplish. Becomes the mission's first user message unless the dispatching skill's on_start overrides it."},
    "inputs":     {"description": "Optional JSON the parent passes alongside the goal — schemas, anchors, prior context."},
    "skill":      {"type": "string", "description": "Skill that provides the mission coordinator pattern (e.g. analyst). Must be a mission-eligible skill (metadata.hugen.mission.enabled:true)."},
    "role":       {"type": "string", "description": "Role within the skill. Optional."},
    "wait":       {"type": "string", "enum": ["sync", "async", "timeout"], "description": "Sync (default): block on the mission's wait_subagents and return its result. Async: return immediately; completion lands as a system message at the next turn boundary. Timeout: like sync but bounded by timeout_ms — partial result if the timer fires first."},
    "timeout_ms": {"type": "integer", "minimum": 1, "description": "Required iff wait=\"timeout\". Soft deadline after which the tool returns a running shape; the mission continues and its completion is delivered as in async mode."},
    "on_complete": {"type": "string", "enum": ["notify", "silent"], "description": "Applies to async / timeout. notify (default): render the completion via interrupts/async_mission_completed.tmpl at the next turn boundary. silent: persist the event without surfacing to the model's history."}
  },
  "required": ["goal"]
}`

type spawnMissionInput struct {
	Goal       string `json:"goal"`
	Inputs     any    `json:"inputs,omitempty"`
	Skill      string `json:"skill,omitempty"`
	Role       string `json:"role,omitempty"`
	Wait       string `json:"wait,omitempty"`
	TimeoutMs  int    `json:"timeout_ms,omitempty"`
	OnComplete string `json:"on_complete,omitempty"`
}

// spawnMissionWaitMode normalises the caller's `wait` argument.
const (
	spawnMissionWaitSync    = "sync"
	spawnMissionWaitAsync   = "async"
	spawnMissionWaitTimeout = "timeout"
)

// spawnMissionResult is the result envelope spawn_mission returns
// — superset of spawnSubagentResult so existing sync-mode callers
// continue parsing the same fields. Status is always set; Result
// is populated only on sync / timeout-completed paths.
type spawnMissionResult struct {
	MissionID string `json:"mission_id"`
	SessionID string `json:"session_id"`
	Depth     int    `json:"depth"`
	Status    string `json:"status"`
	Result    string `json:"result,omitempty"`
	Reason    string `json:"reason,omitempty"`
	TurnsUsed int    `json:"turns_used,omitempty"`
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
	wait := strings.TrimSpace(in.Wait)
	if wait == "" {
		wait = spawnMissionWaitSync
	}
	switch wait {
	case spawnMissionWaitSync, spawnMissionWaitAsync, spawnMissionWaitTimeout:
	default:
		return toolErr("bad_request",
			fmt.Sprintf("wait must be one of sync|async|timeout; got %q", in.Wait))
	}
	if wait == spawnMissionWaitTimeout && in.TimeoutMs <= 0 {
		return toolErr("bad_request",
			"timeout_ms must be > 0 when wait=\"timeout\"")
	}
	onComplete := strings.TrimSpace(in.OnComplete)
	if wait != spawnMissionWaitSync && onComplete == "" {
		onComplete = "notify"
	}
	switch onComplete {
	case "", "notify", "silent":
	default:
		return toolErr("bad_request",
			fmt.Sprintf("on_complete must be notify or silent; got %q", in.OnComplete))
	}
	if wait == spawnMissionWaitAsync {
		if errFrame := enforceAsyncMissionCap(parent); errFrame != nil {
			return errFrame, nil
		}
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

	parent.logger.Debug("session: spawn_mission: entry",
		"parent", parent.id,
		"skill_arg", in.Skill,
		"resolved_skill", skillName,
		"goal_len", len(in.Goal))

	// Resolve on_start block — produces rendered plan body /
	// whiteboard flag / first-message override.
	block := resolveMissionStartBlock(ctx, parent, skillName, in.Goal, in.Inputs)
	if block != nil {
		parent.logger.Debug("session: spawn_mission: on_start resolved",
			"parent", parent.id,
			"skill", skillName,
			"plan_text_len", len(block.PlanText),
			"plan_current_step", block.PlanCurrentStep,
			"whiteboard_init", block.WhiteboardInit,
			"first_message_override_len", len(block.FirstMessageOverride))
	} else {
		parent.logger.Debug("session: spawn_mission: no on_start block",
			"parent", parent.id, "skill", skillName)
	}

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

	// Tag the child's terminal SubagentResult with the projection
	// hint the parent's pump will copy into the payload. Async +
	// timeout-with-notify produce async-completed renders; silent
	// suppresses history projection (event still persisted).
	switch {
	case wait == spawnMissionWaitSync:
		// default render mode
	case onComplete == "silent":
		child.asyncSpawnMode = protocol.SubagentRenderSilent
	default:
		child.asyncSpawnMode = protocol.SubagentRenderAsyncNotify
	}

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

	switch wait {
	case spawnMissionWaitAsync:
		// Return immediately with running shape; the parent pump
		// will deliver the SubagentResult through pendingInbound
		// at next mission completion.
		return json.Marshal(spawnMissionResult{
			MissionID: child.ID(),
			SessionID: child.ID(),
			Depth:     child.depth,
			Status:    "running",
		})
	case spawnMissionWaitTimeout:
		// Block on wait_subagents up to timeout_ms; on deadline
		// the mission keeps running and its terminal result is
		// delivered via the async path.
		return waitForMission(ctx, parent, child, in.TimeoutMs)
	}

	// Sync mode (default + backward compat): return immediately
	// with the legacy shape. The caller (model) is responsible
	// for calling wait_subagents separately. Phase 5.1's tighter
	// "sync = internal wait" semantics are deferred until scenario
	// tuning in κ can validate the round-trip latency for weak
	// models; until then the legacy contract is preserved so
	// existing scenarios are not regressed.
	return json.Marshal(spawnSubagentResult{
		SessionID: child.ID(),
		Depth:     child.depth,
	})
}

// enforceAsyncMissionCap implements § 4.5 — walk to the root via
// the parent chain, count len(root.children), reject when the cap
// is hit. No Manager state; restart is a no-op (RestoreActive re-
// attaches surviving subagents). The cap counts both async AND
// sync children; sync calls naturally block their caller in
// wait_subagents so they can't exceed via spawn loops from the
// same site, while async accumulates linearly until completion
// trims root.children.
func enforceAsyncMissionCap(parent *Session) json.RawMessage {
	if parent.deps == nil || parent.deps.MaxAsyncMissionsPerRoot <= 0 {
		return nil
	}
	cap := parent.deps.MaxAsyncMissionsPerRoot
	root := parent
	for root.parent != nil {
		root = root.parent
	}
	root.childMu.Lock()
	n := len(root.children)
	root.childMu.Unlock()
	if n >= cap {
		out, _ := toolErr("too_many_async",
			fmt.Sprintf("root has %d active children; cap is %d", n, cap))
		return out
	}
	return nil
}

// waitForMission is the timeout result-collection path. Wraps
// callWaitSubagents in a ctx-with-timeout so the call returns
// early on the deadline; on timeout the mission keeps running
// and its terminal result is delivered through the async path
// (with the render mode the caller selected via on_complete).
func waitForMission(ctx context.Context, parent, child *Session,
	timeoutMs int) (json.RawMessage, error) {
	waitCtx, cancel := context.WithTimeout(ctx,
		time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()
	waitArgs, _ := json.Marshal(waitSubagentsInput{IDs: []string{child.ID()}})
	raw, err := parent.callWaitSubagents(waitCtx, waitArgs)
	if err != nil {
		return raw, err
	}
	// Two return shapes: an interrupt envelope (from γ) or the
	// canonical slice of waitResultRow. Sniff the JSON shape to
	// pick the right unwrap.
	var interrupt waitInterruptResult
	if err := json.Unmarshal(raw, &interrupt); err == nil && interrupt.Interrupted {
		// Interrupt during sync spawn: pass-through to the caller
		// so the model handles the follow-up. The spawned mission
		// keeps running; its terminal result lands through the
		// pump on next completion.
		return raw, nil
	}
	var rows []waitResultRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		// Tool error envelope — propagate verbatim.
		return raw, nil
	}
	if len(rows) == 0 {
		// Timeout fired before completion. Mission keeps running.
		return json.Marshal(spawnMissionResult{
			MissionID: child.ID(),
			SessionID: child.ID(),
			Depth:     child.depth,
			Status:    "running",
			Reason:    "timeout",
		})
	}
	row := rows[0]
	return json.Marshal(spawnMissionResult{
		MissionID: row.SessionID,
		SessionID: row.SessionID,
		Depth:     child.depth,
		Status:    row.Status,
		Result:    row.Result,
		Reason:    row.Reason,
		TurnsUsed: row.TurnsUsed,
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
		var ran bool
		for _, ext := range parent.deps.Extensions {
			writer, ok := ext.(extension.PlanSystemWriter)
			if !ok {
				continue
			}
			ran = true
			if err := writer.SystemSet(ctx, child, block.PlanText, block.PlanCurrentStep); err != nil {
				parent.logger.Warn("session: spawn_mission: plan.SystemSet failed",
					"parent", parent.id, "child", child.id, "err", err)
			} else {
				parent.logger.Debug("session: spawn_mission: plan.SystemSet applied",
					"parent", parent.id, "child", child.id,
					"plan_text_len", len(block.PlanText),
					"current_step", block.PlanCurrentStep)
			}
			break
		}
		if !ran {
			parent.logger.Warn("session: spawn_mission: no PlanSystemWriter registered; plan body discarded",
				"parent", parent.id, "child", child.id)
		}
	}
	if block.WhiteboardInit {
		var ran bool
		for _, ext := range parent.deps.Extensions {
			writer, ok := ext.(extension.WhiteboardSystemWriter)
			if !ok {
				continue
			}
			ran = true
			if err := writer.SystemInit(ctx, child); err != nil {
				parent.logger.Warn("session: spawn_mission: whiteboard.SystemInit failed",
					"parent", parent.id, "child", child.id, "err", err)
			} else {
				parent.logger.Debug("session: spawn_mission: whiteboard.SystemInit applied",
					"parent", parent.id, "child", child.id)
			}
			break
		}
		if !ran {
			parent.logger.Warn("session: spawn_mission: no WhiteboardSystemWriter registered; init skipped",
				"parent", parent.id, "child", child.id)
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
