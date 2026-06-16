// Package scheduler hosts the Phase 6 TaskManager per-session
// extension + the always-on `task_log_reap_stuck` system runner
// (see reap.go). The extension's 6.1b shape is intentionally narrow:
// it exposes the `schedule:create` tool so operators / the future
// `_task_builder` mission can persist task rows + the initial
// `planned` row into hub.db. Fire dispatch, drift detection, and
// the pause / resume / cancel / list surface land in 6.1c — those
// tools advertise here as stubs returning a structured "not_yet"
// error so manifests can already include them in their allow-lists.
package scheduler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hugr-lab/hugen/pkg/extension"
	taskext "github.com/hugr-lab/hugen/pkg/extension/task"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/scheduler/runner"
	schedstore "github.com/hugr-lab/hugen/pkg/scheduler/store"
	"github.com/hugr-lab/hugen/pkg/skill"
	"github.com/hugr-lab/hugen/pkg/tool"
)

const providerName = "schedule"

// Permission objects gated by Tier-1 operator config + Tier-2 Hugr
// role rules + Tier-3 per-user policies. The extension exposes the
// full surface; per-tier narrowing is the manifest's job.
const (
	PermCreate = "hugen:schedule:create"
	PermList   = "hugen:schedule:list"
	PermPause  = "hugen:schedule:pause"
	PermResume = "hugen:schedule:resume"
	PermCancel = "hugen:schedule:cancel"
)

// Extension is the per-session TaskManager. Constructed once at
// runtime boot with a [schedstore.TaskStore] + the agent's id;
// every session that loads the `scheduler` extension gets the same
// instance — task scope is enforced at the database layer through
// `tasks.owner_session_id`.
//
// Phase 6.1c adds two runtime dependencies (SessionHost + Runner)
// that aren't available at construction time (the runtime builds
// extensions BEFORE the session manager + runner). Wire them via
// [Extension.Bind] before the first session opens; calls into the
// fire-dispatch path that arrive while runtime/host are nil
// degrade to no-ops + warning logs rather than crashing.
type Extension struct {
	store   schedstore.TaskStore
	skills  *skill.SkillManager
	agentID string
	logger  *slog.Logger

	mu     sync.RWMutex
	host   SessionHost
	runner runner.Runner

	// runRecipe is the shared recipe-execute helper the spawn-fire path
	// delegates to (the task extension's RunRecipe). Wired at boot via
	// BindRunRecipe once both extensions exist. Nil until then — a
	// spawn fire dispatched before wiring fails fast with a terminal
	// log rather than silently never kicking the child.
	runRecipe func(context.Context, taskext.RunParams) (taskext.RunResult, error)

	// bootstrappedSessions tracks sessions whose owned tasks have
	// already been registered with the Runner, so InitState (called
	// once per session by the runtime) AND any later force-replays
	// stay idempotent. Sessions are added on first InitState; the
	// extension never removes them — tasks are unregistered
	// individually on cancel / session-close.
	bootstrappedSessions map[string]struct{}

	// pendingFires is the FireContext rendezvous between the fire
	// dispatch goroutine (writer) and [Extension.ApplyOnSubagentSpawn]
	// (reader). The key is the sanitised SpawnSpec.Name we hand to
	// pkg/session.Spawn; the applier looks up by [SessionState.SubagentName]
	// and stamps the value on the child's session state under
	// [protocol.SchedulerFireStateKey].
	pendingFires sync.Map // map[string]*protocol.FireContext

	// spawnCounter generates monotonic unique tokens for cron-spawn
	// names so concurrent fires under the same owner can't collide.
	// Resets to zero on process restart — that's fine, the map is
	// also in-memory and gets emptied on restart.
	spawnCounter atomic.Int64
}

// NewExtension constructs the TaskManager extension. The
// SkillManager pointer is used by `schedule:create` to validate skill
// references + read `task.inputs_schema` for Phase 6.1c JSON-Schema
// validation; today the 6.1b path only confirms the skill exists.
func NewExtension(st schedstore.TaskStore, skills *skill.SkillManager, agentID string, logger *slog.Logger) *Extension {
	if logger == nil {
		logger = slog.Default()
	}
	return &Extension{
		store:                st,
		skills:               skills,
		agentID:              agentID,
		logger:               logger,
		bootstrappedSessions: make(map[string]struct{}),
	}
}

// Bind installs the runtime dependencies the fire-dispatch path
// needs: the [SessionHost] for opening cron sessions / delivering
// wake messages / lookups, and the [runner.Runner] for registering
// each user task at session-open time. Called once at boot AFTER
// the runtime's session manager + runner have been constructed
// (see pkg/runtime/runner.go phaseRunner).
//
// Calling Bind twice is allowed (idempotent overwrite) so future
// hot-reload paths can swap implementations without recreating the
// extension. Calling Bind with nil host or nil runner leaves the
// existing reference in place — pass concrete deps every time, or
// the extension stays in its un-bound state and InitState skips
// bootstrap.
func (e *Extension) Bind(host SessionHost, r runner.Runner) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if host != nil {
		e.host = host
	}
	if r != nil {
		e.runner = r
	}
}

// BindRunRecipe installs the shared recipe-execute helper the spawn-fire
// path delegates to — the task extension's RunRecipe, which spawns the
// recipe child, scopes its skill surface, pre-loads the recipe body,
// KICKS the first turn, and waits for termination. Wired at boot once
// both extensions exist (pkg/runtime/runner.go), after Bind. The kick is
// the load-bearing half: the prior cron-as-subagent path spawned a child
// but never delivered a first UserMessage, so the model loop never fired
// (B46). Idempotent overwrite; a nil fn leaves the existing reference.
func (e *Extension) BindRunRecipe(fn func(context.Context, taskext.RunParams) (taskext.RunResult, error)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if fn != nil {
		e.runRecipe = fn
	}
}

// runtimeBound returns true when both the SessionHost AND the
// Runner are set on the extension. Bootstrap + tool dispatch
// short-circuit when this is false (early boot, tests).
func (e *Extension) runtimeBound() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.host != nil && e.runner != nil
}

// Compile-time interface assertions.
var (
	_ extension.Extension             = (*Extension)(nil)
	_ tool.ToolProvider               = (*Extension)(nil)
	_ extension.ToolApprovalPolicy    = (*Extension)(nil)
	_ extension.InquiryPolicy         = (*Extension)(nil)
	_ extension.SubagentSpawnApplier  = (*Extension)(nil)
)

// stashFire records a FireContext for the named pending cron spawn.
// [Extension.ApplyOnSubagentSpawn] reads + stamps + clears on the
// child's first state init step (after Session.Spawn returns but
// before the task UserMessage lands in the child's inbox). The
// scheduler clears via [releaseFire] as a belt-and-braces guard
// against an applier that didn't fire (e.g. spawn errored out
// mid-flight before the runtime reached the applier sweep).
func (e *Extension) stashFire(spawnName string, fc *protocol.FireContext) {
	if fc == nil || spawnName == "" {
		return
	}
	e.pendingFires.Store(spawnName, fc)
}

// releaseFire drops the entry for spawnName. No-op if missing.
func (e *Extension) releaseFire(spawnName string) {
	if spawnName == "" {
		return
	}
	e.pendingFires.Delete(spawnName)
}

// takeSpawnToken returns a monotonic per-process token used to
// disambiguate cron spawn names so concurrent fires under the same
// owner can't reuse a [SpawnSpec.Name].
func (e *Extension) takeSpawnToken() int64 {
	return e.spawnCounter.Add(1)
}

// ApplyOnSubagentSpawn implements [extension.SubagentSpawnApplier].
// Recognises a scheduler-owned spawn by matching the child's
// SubagentName against the pending-fires map. On hit stamps
// FireContext on the child state — this is the moment the cron
// system prompt advertiser + the CronApprovalPolicy become aware
// of the fire envelope. Non-scheduler spawns no-op silently.
//
// Runs AFTER Session.Spawn returns + intent hint applied + BEFORE
// the task UserMessage lands. See [extension.SubagentSpawnApplier]
// for the run-once contract.
func (e *Extension) ApplyOnSubagentSpawn(_ context.Context, child extension.SessionState, _ string, _ string) error {
	if child == nil {
		return nil
	}
	name := child.SubagentName()
	if name == "" {
		return nil
	}
	v, ok := e.pendingFires.Load(name)
	if !ok {
		return nil
	}
	fc, _ := v.(*protocol.FireContext)
	if fc == nil {
		return nil
	}
	child.SetValue(protocol.SchedulerFireStateKey, fc)
	return nil
}

func (e *Extension) Name() string            { return providerName }
func (e *Extension) Lifetime() tool.Lifetime { return tool.LifetimePerAgent }

// InitState reads tasks owned by the calling session from the
// store and registers a fire fn with the Runner for each active
// row. Idempotent per session — the extension tracks
// bootstrappedSessions so a re-entry (resume path, theoretical
// recovery sweep) doesn't double-register.
//
// Pre-bind state (host / runner nil): InitState returns nil
// silently. The runtime calls Bind from phaseRunner BEFORE the
// session manager starts opening sessions, so production-path
// InitState always sees a bound extension. Tests that don't wire
// the runtime can construct the extension without Bind and
// InitState stays a safe no-op.
//
// Cron-spawned sessions skip bootstrap entirely: they're
// transient fire vessels, not task owners. The
// [protocol.SchedulerFireStateKey] presence is the discriminator.
func (e *Extension) InitState(ctx context.Context, state extension.SessionState) error {
	if state == nil {
		return nil
	}
	if !e.runtimeBound() {
		return nil
	}
	if _, isCron := fireContextFromState(state); isCron {
		return nil
	}
	sessionID := state.SessionID()
	if sessionID == "" {
		return nil
	}
	e.mu.Lock()
	if _, ok := e.bootstrappedSessions[sessionID]; ok {
		e.mu.Unlock()
		return nil
	}
	e.bootstrappedSessions[sessionID] = struct{}{}
	e.mu.Unlock()

	if e.store == nil {
		return nil
	}
	rows, err := e.store.ListTasksBySession(ctx, sessionID, schedstore.ListTasksOpts{})
	if err != nil {
		e.logger.Warn("scheduler: bootstrap ListTasksBySession failed",
			"session", sessionID, "err", err)
		return nil
	}
	for _, row := range rows {
		if err := e.registerTask(ctx, row); err != nil {
			e.logger.Warn("scheduler: bootstrap register failed",
				"task_id", row.ID, "session", sessionID, "err", err)
		}
	}
	return nil
}

// registerTask installs (or replaces) the runner registration for
// a task row. The next fire is anchored on `task_log`'s
// LatestPlannedFire so a restart resumes against the persisted
// schedule rather than starting from scratch. Cancelled +
// completed rows are skipped (no point firing them); paused rows
// register with [runner.WithStartPaused] so resume is a single
// runner.Resume call.
func (e *Extension) registerTask(ctx context.Context, row schedstore.TaskRow) error {
	if !e.runtimeBound() {
		return fmt.Errorf("scheduler: registerTask before Bind")
	}
	if row.Status == schedstore.StatusCancelled || row.Status == schedstore.StatusCompleted {
		return nil
	}
	planned, err := e.store.LatestPlannedFire(ctx, row.ID)
	if err != nil {
		return fmt.Errorf("LatestPlannedFire: %w", err)
	}
	if planned == nil {
		return fmt.Errorf("task %s has no planned row in task_log", row.ID)
	}
	sched := runner.Once(planned.PlannedAt)

	deps := e.fireDeps()
	fn := buildFireFn(row, deps)
	opts := []runner.RegisterOption{}
	if row.Status == schedstore.StatusPaused {
		opts = append(opts, runner.WithStartPaused())
	}
	return e.runner.Register(ctx, runnerNameForTask(row.ID), sched, fn, opts...)
}

// fireDeps materialises the closure-captured deps each fire fn
// holds. Snapshotted under the read lock so a concurrent Bind
// can't tear the references mid-build.
func (e *Extension) fireDeps() fireDeps {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return fireDeps{
		store:          e.store,
		host:           e.host,
		skills:         e.skills,
		agentID:        e.agentID,
		logger:         e.logger,
		registerFn:     e.reRegisterFn(),
		pauseFn:        e.pauseFnLocked(),
		stashFire:      e.stashFire,
		releaseFire:    e.releaseFire,
		takeSpawnToken: e.takeSpawnToken,
		runRecipe:      e.runRecipe,
	}
}

// pauseFnLocked returns a closure that pauses the named task on
// the bound runner. Captures the runner reference once so a later
// Bind swap doesn't change behaviour mid-fire. MUST be called
// under e.mu (read or write).
func (e *Extension) pauseFnLocked() func(string) error {
	r := e.runner
	if r == nil {
		return nil
	}
	return func(taskID string) error {
		return r.Pause(context.Background(), runnerNameForTask(taskID))
	}
}

// reRegisterFn returns a closure the fire fn calls to install the
// next planned fire's schedule after appending its `planned` row.
// Captures `e.runner` once so the closure stays independent of any
// later Bind swap.
//
// MUST be called under e.mu (read or write). Callers that don't
// hold the lock should funnel through fireDeps().
func (e *Extension) reRegisterFn() func(string, runner.Schedule) error {
	r := e.runner
	if r == nil {
		return nil
	}
	deps := struct {
		runner   runner.Runner
		buildFn  func(string) (runner.RunnerFn, error)
	}{
		runner: r,
		buildFn: func(taskID string) (runner.RunnerFn, error) {
			row, err := e.store.GetTask(context.Background(), taskID)
			if err != nil {
				return nil, err
			}
			return buildFireFn(row, e.fireDeps()), nil
		},
	}
	return func(taskID string, sched runner.Schedule) error {
		fn, err := deps.buildFn(taskID)
		if err != nil {
			return err
		}
		return deps.runner.Register(context.Background(), runnerNameForTask(taskID), sched, fn)
	}
}

// runnerNameForTask is the canonical runner-registration name for
// a task. The "task_" prefix lets [runner.ListByPrefix] enumerate
// every user task without colliding with the system reaper
// prefixes ("sessions_", "subagents_", "task_log_").
func runnerNameForTask(taskID string) string {
	return "task_" + taskID
}

// rootOf walks state.Parent() upward until it reaches a session
// with no parent — the root of the tree. The caller's session may
// itself be the root (parent == nil); we return it as-is.
//
// Phase 6.1c uses this in [Extension.callCreate] so the persisted
// `owner_session_id` always names a root session, not an
// intermediate worker / mission. Fire dispatch then spawns the
// cron subagent under THAT root via owner.Spawn — predictable
// depth + stable owner across long-running tasks even when the
// originating worker terminates.
func rootOf(state extension.SessionState) extension.SessionState {
	cur := state
	for {
		parent, ok := cur.Parent()
		if !ok || parent == nil {
			return cur
		}
		cur = parent
	}
}

const createSchema = `{
  "type": "object",
  "properties": {
    "skill_ref":     {"type": "string", "description": "Skill name to load on each fire (spawn kind). Leave empty for wake kind."},
    "kind":          {"type": "string", "enum": ["wake", "spawn"], "description": "Fire delivery kind. 'wake' = synthetic UserMessage into owner session; 'spawn' = cron-fire subagent under the owner per fire."},
    "schedule_kind": {"type": "string", "enum": ["once_in", "once_at", "cron", "interval"], "description": "Schedule shape; semantics keyed by schedule_spec."},
    "schedule_spec": {"type": "string", "description": "Schedule expression — Go duration ('5m', '24h') for once_in/interval; RFC3339 timestamp for once_at; 5-field cron expression ('0 9 * * 1' = Mon 09:00) for cron."},
    "timezone":      {"type": "string", "description": "IANA location name (e.g. 'Europe/Berlin') a cron schedule_spec is evaluated in. Defaults to UTC. Only used by schedule_kind=cron."},
    "name":          {"type": "string", "description": "Short label for UI / logs."},
    "description":   {"type": "string", "description": "User's intent in words. Optional."},
    "goal":          {"type": "string", "description": "Imperative one-line brief for spawn fires."},
    "wake_message":  {"type": "string", "description": "Synthetic UserMessage body for wake fires. Treated as a Go text/template against FireRenderContext."},
    "allowed_tools": {"type": "array", "items": {"type": "string"}, "description": "Per-task tool allow-list frozen at create time. CronApprovalPolicy auto-approves matching tools during each cron fire so the worker never blocks on a modal the absent operator can't answer."},
    "inputs":        {"type": "object", "description": "Structured inputs blob — interpolated into the skill body / goal at fire time via the FireRenderContext templates."},
    "initial_planned_at": {"type": "string", "description": "Optional RFC3339 UTC override for the first fire instant. When omitted the runtime derives it from schedule_spec: now+duration for once_in/interval, the timestamp itself for once_at."},
    "end_condition": {
      "type": "object",
      "description": "Optional — defaults to {kind:'until_cancel'}. For once_in/once_at the schedule itself is one-shot so this field is ignored.",
      "properties": {
        "kind": {"type": "string", "enum": ["until_cancel", "count", "until"]},
        "spec": {"type": "string"}
      },
      "required": ["kind"]
    }
  },
  "required": ["kind", "schedule_kind", "schedule_spec", "name"]
}`

// listSchema documents `schedule:list` — no required fields; optional
// filters narrow by status / cap result count.
const listSchema = `{
  "type": "object",
  "properties": {
    "status": {"type": "string", "enum": ["active", "paused", "cancelled", "completed"], "description": "Filter to tasks in this status. Omitted = all."},
    "limit":  {"type": "integer", "description": "Cap result rows. <= 0 uses the store default."}
  }
}`

// taskIDSchema is the input shape for pause / resume / cancel.
const taskIDSchema = `{
  "type": "object",
  "properties": {
    "task_id": {"type": "string", "description": "Source task id from a prior schedule:create / schedule:list response."},
    "reason":  {"type": "string", "description": "Pause-reason category (pause only). Defaults to 'user'."}
  },
  "required": ["task_id"]
}`

// List implements [tool.ToolProvider]. Returns the scheduler-tier
// management surface: create / list / pause / resume / cancel.
// Recipe execution is a sibling concern owned by the task
// extension (`pkg/extension/task`) — that ext emits one synthetic
// `task:<recipe-name>` tool per task-eligible skill and dispatches
// the spawn. Phase 6.1d.
func (e *Extension) List(_ context.Context) ([]tool.Tool, error) {
	return []tool.Tool{
		{
			Name:             providerName + ":create",
			Description:      "Create a scheduled task. Persists a `tasks` row + the initial `task_log` planned row atomically. Phase 6.1b — fire dispatch lands in 6.1c; rows created here remain visible via GraphQL but are not yet executed.",
			Provider:         providerName,
			PermissionObject: PermCreate,
			ArgSchema:        json.RawMessage(createSchema),
		},
		{
			Name:             providerName + ":list",
			Description:      "List tasks owned by the current session. Each entry carries the task id, schedule kind / spec, status, and next planned-at instant.",
			Provider:         providerName,
			PermissionObject: PermList,
			ArgSchema:        json.RawMessage(listSchema),
		},
		{
			Name:             providerName + ":pause",
			Description:      "Pause a task (status='paused'). Future fires skip until schedule:resume; current in-flight fire (if any) is allowed to finish.",
			Provider:         providerName,
			PermissionObject: PermPause,
			ArgSchema:        json.RawMessage(taskIDSchema),
		},
		{
			Name:             providerName + ":resume",
			Description:      "Resume a paused task. Re-anchors the runner schedule on the latest planned task_log row.",
			Provider:         providerName,
			PermissionObject: PermResume,
			ArgSchema:        json.RawMessage(taskIDSchema),
		},
		{
			Name:             providerName + ":cancel",
			Description:      "Cancel a task (status='cancelled'). Removes the runner registration; the task row stays for audit.",
			Provider:         providerName,
			PermissionObject: PermCancel,
			ArgSchema:        json.RawMessage(taskIDSchema),
		},
	}, nil
}

// Call implements [tool.ToolProvider].
func (e *Extension) Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	short := stripProviderPrefix(name)
	switch short {
	case "create":
		return e.callCreate(ctx, args)
	case "list":
		return e.callList(ctx, args)
	case "pause":
		return e.callPause(ctx, args)
	case "resume":
		return e.callResume(ctx, args)
	case "cancel":
		return e.callCancel(ctx, args)
	default:
		return nil, fmt.Errorf("%w: task:%s", tool.ErrUnknownTool, short)
	}
}

func (e *Extension) Subscribe(_ context.Context) (<-chan tool.ProviderEvent, error) {
	return nil, nil
}

func (e *Extension) Close() error { return nil }

func stripProviderPrefix(name string) string {
	pfx := providerName + ":"
	if len(name) > len(pfx) && name[:len(pfx)] == pfx {
		return name[len(pfx):]
	}
	return name
}

// createInput is the shape `schedule:create` accepts. Field order matches
// the JSON Schema declared in createSchema above. Optional fields are
// omitted via pointer or zero-value sentinels.
type createInput struct {
	SkillRef         string                       `json:"skill_ref,omitempty"`
	Kind             string                       `json:"kind"`
	ScheduleKind     string                       `json:"schedule_kind"`
	ScheduleSpec     string                       `json:"schedule_spec"`
	Timezone         string                       `json:"timezone,omitempty"`
	InitialPlannedAt string                       `json:"initial_planned_at"`
	Name             string                       `json:"name"`
	Description      string                       `json:"description,omitempty"`
	Goal             string                       `json:"goal,omitempty"`
	WakeMessage      string                       `json:"wake_message,omitempty"`
	AllowedTools     []string                     `json:"allowed_tools,omitempty"`
	Inputs           map[string]any               `json:"inputs,omitempty"`
	EndCondition     schedstore.TaskEndCondition `json:"end_condition"`
}

// createOutput is the wire shape returned to the model on success.
type createOutput struct {
	TaskID           string `json:"task_id"`
	InitialPlannedAt string `json:"initial_planned_at"`
	Status           string `json:"status"`
}

func (e *Extension) callCreate(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if e.store == nil {
		return toolErr("not_yet_implemented", "TaskStore is not wired — Phase 6.1b deployment incomplete")
	}
	var in createInput
	if err := json.Unmarshal(args, &in); err != nil {
		return toolErr("invalid_args", fmt.Sprintf("decode: %v", err))
	}
	if in.Kind != schedstore.KindWake && in.Kind != schedstore.KindSpawn {
		return toolErr("invalid_args", fmt.Sprintf("kind must be wake|spawn (got %q)", in.Kind))
	}
	if in.Kind == schedstore.KindSpawn && in.SkillRef == "" {
		return toolErr("invalid_args", "spawn kind requires skill_ref")
	}
	if in.Kind == schedstore.KindWake && in.WakeMessage == "" {
		return toolErr("invalid_args", "wake kind requires wake_message")
	}
	if in.ScheduleSpec == "" {
		return toolErr("invalid_args", "schedule_spec required")
	}
	// Validate the cron expression + timezone at create time so a bad
	// spec is rejected up front — including the initial_planned_at
	// override path, where resolveInitialPlannedAt returns early
	// without consulting the expression. Recompute would otherwise
	// pause the task on its first fire.
	if in.ScheduleKind == schedstore.ScheduleCron {
		loc, lerr := resolveLocation(in.Timezone)
		if lerr != nil {
			return toolErr("invalid_args", lerr.Error())
		}
		if _, cerr := runner.Cron(in.ScheduleSpec, loc); cerr != nil {
			return toolErr("invalid_args", fmt.Sprintf("schedule_spec %v", cerr))
		}
	}
	planned, err := resolveInitialPlannedAt(in)
	if err != nil {
		return toolErr("invalid_args", err.Error())
	}
	// end_condition defaults to until_cancel; for once_in/once_at
	// the schedule itself is one-shot and maybeScheduleNext cancels
	// the task on the first fire regardless of this value.
	if in.EndCondition.Kind == "" {
		in.EndCondition.Kind = "until_cancel"
	}

	// Skill existence check (6.1b minimum). Full task.inputs_schema
	// JSON-Schema validation lands in 6.1c.
	if in.SkillRef != "" && e.skills != nil {
		sk, err := e.skills.Get(ctx, in.SkillRef)
		if err != nil {
			return toolErr("unknown_skill", fmt.Sprintf("skill %q not in catalog: %v", in.SkillRef, err))
		}
		if !sk.Manifest.Hugen.Task.Eligible {
			return toolErr("not_task_eligible", fmt.Sprintf("skill %q is not task-eligible (metadata.hugen.task.eligible)", in.SkillRef))
		}
		if sk.Manifest.Hugen.Task.Kind == skill.TaskKindMission {
			return toolErr("not_yet_implemented", "mission-shape tasks are reserved — MVP supports kind=worker only")
		}

		// B47 step 3 — a scheduled task fires HEADLESS: no human at fire
		// time to supply inputs or a goal. Both must be resolved + frozen
		// NOW, or the schedule is unfireable (a bare skill_ref is not a
		// run). Reject up front so root re-asks the user instead of
		// persisting a schedule that fails / fires idle every tick. Only
		// spawn-kind runs the skill; wake-kind nudges the owner and
		// carries no inputs/goal.
		if in.Kind == schedstore.KindSpawn {
			if miss := missingRequiredInputs(sk.Manifest.Hugen.Task.InputsSchema, in.Inputs); len(miss) > 0 {
				return toolErr("missing_inputs", fmt.Sprintf(
					"task %q requires inputs [%s] — a scheduled task has no fire-time prompt, so set them in `inputs` now",
					in.SkillRef, strings.Join(miss, ", ")))
			}
			// Freeze the launch goal: explicit `goal` wins, else the
			// task's declared goal_summary. Resolved NOW so every fire
			// has a kick body (the cron path uses Spec.Goal verbatim).
			if strings.TrimSpace(in.Goal) == "" {
				in.Goal = strings.TrimSpace(sk.Manifest.Hugen.Task.GoalSummary)
			}
			if in.Goal == "" {
				return toolErr("missing_goal", fmt.Sprintf(
					"task %q has no goal — set `goal`, or the task must declare a goal_summary", in.SkillRef))
			}
			// Freeze the task's standardized tool set when the caller did
			// not narrow it: a scheduled task fires headless, and the cron
			// path blanket-auto-approves its tools, so the frozen list is
			// the audit/visibility record of what the unattended fire may
			// run (§5.1). An explicit allowed_tools on the call wins.
			if len(in.AllowedTools) == 0 {
				in.AllowedTools = append([]string(nil), sk.Manifest.Hugen.Task.AllowedToolsDefault...)
			}
		}
	}

	state, ok := extension.SessionStateFromContext(ctx)
	if !ok {
		return toolErr("internal_error", "no session state in context")
	}

	// Owner = ROOT of the caller's tree so fire dispatch can spawn
	// subagents predictably from a long-lived root regardless of
	// who actually invoked schedule:create (could be a worker inside a
	// mission). Walking up here is cheap (depth is typically <=3)
	// and keeps the cron subagent depth invariant at root.depth+1.
	owner := rootOf(state)

	taskID := newTaskID(e.agentID)
	row := schedstore.TaskRow{
		ID:             taskID,
		AgentID:        e.agentID,
		Kind:           in.Kind,
		Status:         schedstore.StatusActive,
		ScheduleKind:   in.ScheduleKind,
		OwnerSessionID: owner.SessionID(),
		SkillRef:       in.SkillRef,
		Spec: schedstore.TaskSpec{
			Name:         in.Name,
			Description:  in.Description,
			ScheduleSpec: in.ScheduleSpec,
			Timezone:     in.Timezone,
			EndCondition: in.EndCondition,
			Goal:         in.Goal,
			WakeMessage:  in.WakeMessage,
			AllowedTools: in.AllowedTools,
			Inputs:       in.Inputs,
		},
	}
	// Phase 6.1c — stamp drift hashes at create time so a later
	// fire can pause if the source skill changed under us. Skill
	// ref empty (wake-only tasks) → leave hashes empty; the fire
	// fn short-circuits drift detection.
	if in.SkillRef != "" && e.skills != nil {
		if sk, err := e.skills.Get(ctx, in.SkillRef); err == nil {
			row.Spec.Hashes = schedstore.TaskHashes{
				Skill:        hashSkillManifest(sk.Manifest),
				InputsSchema: hashJSON(sk.Manifest.Hugen.Task.InputsSchema),
				Inputs:       hashJSON(in.Inputs),
			}
		}
	}

	if err := e.store.OpenTask(ctx, row, planned.UTC()); err != nil {
		return toolErr("store_error", fmt.Sprintf("OpenTask: %v", err))
	}

	// Phase 6.1c — register the fire fn with the runner so it
	// starts firing on schedule. Failure here is recoverable: the
	// task row + planned log entry are already persisted, so a
	// session restart picks them up via InitState. We log and
	// surface success.
	if e.runtimeBound() {
		if err := e.registerTask(ctx, row); err != nil {
			e.logger.Warn("scheduler: schedule:create register",
				"task_id", taskID, "err", err)
		}
	}

	out := createOutput{
		TaskID:           taskID,
		InitialPlannedAt: planned.UTC().Format(time.RFC3339Nano),
		Status:           schedstore.StatusActive,
	}
	body, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("schedule:create: marshal response: %w", err)
	}
	return body, nil
}

// missingRequiredInputs reports which `required` keys a task's
// inputs_schema declares but the supplied inputs blob does not provide
// (absent or explicit null). A nil/absent `required` array — or a task
// with no inputs_schema — yields no missing keys. The schema's `required`
// decodes to []any (JSON/YAML generic decode); non-string / empty
// entries are skipped defensively. Order follows the schema for stable
// error messages. B47 step 3.
func missingRequiredInputs(schema, inputs map[string]any) []string {
	req, ok := schema["required"].([]any)
	if !ok {
		return nil
	}
	var missing []string
	for _, r := range req {
		key, ok := r.(string)
		if !ok || key == "" {
			continue
		}
		if v, present := inputs[key]; !present || v == nil {
			missing = append(missing, key)
		}
	}
	return missing
}

// listInput / listOutput shape the `schedule:list` surface.
type listInput struct {
	Status string `json:"status,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

type listEntry struct {
	TaskID         string `json:"task_id"`
	Kind           string `json:"kind"`
	Status         string `json:"status"`
	ScheduleKind   string `json:"schedule_kind"`
	ScheduleSpec   string `json:"schedule_spec,omitempty"`
	Name           string `json:"name,omitempty"`
	SkillRef       string `json:"skill_ref,omitempty"`
	NextPlannedAt  string `json:"next_planned_at,omitempty"`
	PauseReason    string `json:"pause_reason,omitempty"`
}

type listOutput struct {
	Tasks []listEntry `json:"tasks"`
}

func (e *Extension) callList(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if e.store == nil {
		return toolErr("not_yet_implemented", "TaskStore is not wired")
	}
	state, ok := extension.SessionStateFromContext(ctx)
	if !ok {
		return toolErr("internal_error", "no session state in context")
	}
	in := listInput{}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return toolErr("invalid_args", fmt.Sprintf("decode: %v", err))
		}
	}
	// Owner = root of caller's tree, matching schedule:create + /tasks.
	owner := rootOf(state)
	rows, err := e.store.ListTasksBySession(ctx, owner.SessionID(), schedstore.ListTasksOpts{
		Status: in.Status,
		Limit:  in.Limit,
	})
	if err != nil {
		return toolErr("store_error", fmt.Sprintf("ListTasksBySession: %v", err))
	}
	out := listOutput{Tasks: make([]listEntry, 0, len(rows))}
	for _, row := range rows {
		entry := listEntry{
			TaskID:       row.ID,
			Kind:         row.Kind,
			Status:       row.Status,
			ScheduleKind: row.ScheduleKind,
			ScheduleSpec: row.Spec.ScheduleSpec,
			Name:         row.Spec.Name,
			SkillRef:     row.SkillRef,
			PauseReason:  row.PauseReason,
		}
		if planned, err := e.store.LatestPlannedFire(ctx, row.ID); err == nil && planned != nil {
			entry.NextPlannedAt = planned.PlannedAt.UTC().Format(time.RFC3339Nano)
		}
		out.Tasks = append(out.Tasks, entry)
	}
	body, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("schedule:list: marshal: %w", err)
	}
	return body, nil
}

// taskIDInput is the shared shape for pause/resume/cancel — a
// single `task_id` reference.
type taskIDInput struct {
	TaskID string `json:"task_id"`
	Reason string `json:"reason,omitempty"`
}

// taskAckOutput acknowledges a one-shot lifecycle command.
type taskAckOutput struct {
	TaskID string `json:"task_id"`
	Status string `json:"status"`
}

func (e *Extension) callPause(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	in, owned, errResp, err := e.resolveOwnedTask(ctx, args)
	if errResp != nil || err != nil {
		return errResp, err
	}
	reason := in.Reason
	if reason == "" {
		reason = schedstore.PauseUser
	}
	if err := e.store.PauseTask(ctx, owned.ID, reason); err != nil {
		return toolErr("store_error", fmt.Sprintf("PauseTask: %v", err))
	}
	if e.runtimeBound() {
		if perr := e.runner.Pause(ctx, runnerNameForTask(owned.ID)); perr != nil {
			e.logger.Warn("scheduler: runner pause", "task_id", owned.ID, "err", perr)
		}
	}
	body, _ := json.Marshal(taskAckOutput{TaskID: owned.ID, Status: schedstore.StatusPaused})
	return body, nil
}

func (e *Extension) callResume(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	in, owned, errResp, err := e.resolveOwnedTask(ctx, args)
	if errResp != nil || err != nil {
		return errResp, err
	}
	_ = in
	if owned.Status == schedstore.StatusCancelled || owned.Status == schedstore.StatusCompleted {
		return toolErr("invalid_state", fmt.Sprintf("cannot resume task in status %q", owned.Status))
	}
	if err := e.store.ResumeTask(ctx, owned.ID); err != nil {
		return toolErr("store_error", fmt.Sprintf("ResumeTask: %v", err))
	}
	if e.runtimeBound() {
		// Re-register so a new schedule anchored on the latest
		// planned row replaces any stale runner-side entry. This
		// covers both "runner forgot us after restart" and
		// "drift-pause cleared the registration" paths.
		owned.Status = schedstore.StatusActive
		if rerr := e.registerTask(ctx, owned); rerr != nil {
			e.logger.Warn("scheduler: re-register on resume", "task_id", owned.ID, "err", rerr)
		}
	}
	body, _ := json.Marshal(taskAckOutput{TaskID: owned.ID, Status: schedstore.StatusActive})
	return body, nil
}

func (e *Extension) callCancel(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	in, owned, errResp, err := e.resolveOwnedTask(ctx, args)
	if errResp != nil || err != nil {
		return errResp, err
	}
	_ = in
	if err := e.store.CancelTask(ctx, owned.ID); err != nil {
		return toolErr("store_error", fmt.Sprintf("CancelTask: %v", err))
	}
	if e.runtimeBound() {
		if uerr := e.runner.Unregister(ctx, runnerNameForTask(owned.ID)); uerr != nil {
			e.logger.Warn("scheduler: runner unregister", "task_id", owned.ID, "err", uerr)
		}
	}
	body, _ := json.Marshal(taskAckOutput{TaskID: owned.ID, Status: schedstore.StatusCancelled})
	return body, nil
}

// resolveOwnedTask validates the caller is the task's owner. Returns
// the parsed input + the loaded task row, or a tool-error response
// (with nil go-error) for any guard failure.
func (e *Extension) resolveOwnedTask(ctx context.Context, args json.RawMessage) (taskIDInput, schedstore.TaskRow, json.RawMessage, error) {
	var in taskIDInput
	if e.store == nil {
		resp, err := toolErr("not_yet_implemented", "TaskStore is not wired")
		return in, schedstore.TaskRow{}, resp, err
	}
	if err := json.Unmarshal(args, &in); err != nil {
		resp, err2 := toolErr("invalid_args", fmt.Sprintf("decode: %v", err))
		return in, schedstore.TaskRow{}, resp, err2
	}
	if in.TaskID == "" {
		resp, err := toolErr("invalid_args", "task_id required")
		return in, schedstore.TaskRow{}, resp, err
	}
	state, ok := extension.SessionStateFromContext(ctx)
	if !ok {
		resp, err := toolErr("internal_error", "no session state in context")
		return in, schedstore.TaskRow{}, resp, err
	}
	row, err := e.store.GetTask(ctx, in.TaskID)
	if err != nil {
		resp, err2 := toolErr("not_found", fmt.Sprintf("task %q: %v", in.TaskID, err))
		return in, row, resp, err2
	}
	// Owner = root of caller's tree, matching schedule:create + /tasks.
	if row.OwnerSessionID != rootOf(state).SessionID() {
		resp, err := toolErr("forbidden", "task is not owned by this session")
		return in, row, resp, err
	}
	return in, row, nil, nil
}

type toolErrorResponse struct {
	Error toolError `json:"error"`
}

type toolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func toolErr(code, msg string) (json.RawMessage, error) {
	body, err := json.Marshal(toolErrorResponse{Error: toolError{Code: code, Message: msg}})
	if err != nil {
		return nil, fmt.Errorf("task: marshal tool error: %w", err)
	}
	return body, nil
}

// resolveInitialPlannedAt computes the first-fire instant from the
// optional InitialPlannedAt override, falling back to deriving it
// from schedule_kind + schedule_spec. The dual-source path keeps
// the tool surface minimal — most callers can omit the override
// and let the runtime do the arithmetic — while still letting an
// operator nail a specific UTC moment when needed (e.g. a recurring
// task that must align to the top of the hour regardless of when
// schedule:create runs).
//
// Returns an error formatted for direct surfacing as the tool's
// invalid_args message body.
func resolveInitialPlannedAt(in createInput) (time.Time, error) {
	if in.InitialPlannedAt != "" {
		t, err := time.Parse(time.RFC3339, in.InitialPlannedAt)
		if err != nil {
			return time.Time{}, fmt.Errorf("initial_planned_at must be RFC3339 (got %q): %v", in.InitialPlannedAt, err)
		}
		return t.UTC(), nil
	}
	now := time.Now().UTC()
	switch in.ScheduleKind {
	case schedstore.ScheduleOnceIn, schedstore.ScheduleInterval:
		d, err := time.ParseDuration(in.ScheduleSpec)
		if err != nil {
			return time.Time{}, fmt.Errorf("schedule_spec must be a Go duration for %q (got %q): %v",
				in.ScheduleKind, in.ScheduleSpec, err)
		}
		if d <= 0 {
			return time.Time{}, fmt.Errorf("schedule_spec duration must be positive for %q (got %q)",
				in.ScheduleKind, in.ScheduleSpec)
		}
		return now.Add(d), nil
	case schedstore.ScheduleOnceAt:
		t, err := time.Parse(time.RFC3339, in.ScheduleSpec)
		if err != nil {
			return time.Time{}, fmt.Errorf("schedule_spec must be RFC3339 for %q (got %q): %v",
				in.ScheduleKind, in.ScheduleSpec, err)
		}
		return t.UTC(), nil
	case schedstore.ScheduleCron:
		loc, err := resolveLocation(in.Timezone)
		if err != nil {
			return time.Time{}, err
		}
		sched, err := runner.Cron(in.ScheduleSpec, loc)
		if err != nil {
			return time.Time{}, fmt.Errorf("schedule_spec %v", err)
		}
		next := sched.Next(now)
		if next.IsZero() {
			return time.Time{}, fmt.Errorf("cron expression %q yields no future fire", in.ScheduleSpec)
		}
		return next, nil
	default:
		return time.Time{}, fmt.Errorf("unknown schedule_kind %q", in.ScheduleKind)
	}
}

// resolveLocation maps an optional IANA timezone name to a
// *time.Location. Empty → UTC. Invalid names surface as an error the
// caller turns into an invalid_args tool response.
func resolveLocation(tz string) (*time.Location, error) {
	if tz == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("invalid timezone %q: %v", tz, err)
	}
	return loc, nil
}

// newTaskID is the synthetic sortable id used for tasks rows. Shape
// `tsk_<agent>_<ts>_<rnd>` matches the conventions on memory_items /
// session_events.
func newTaskID(agentID string) string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("tsk_%s_%d", agentID, time.Now().UnixNano())
	}
	return fmt.Sprintf("tsk_%s_%d_%s", agentID, time.Now().UnixNano(), hex.EncodeToString(b[:]))
}
