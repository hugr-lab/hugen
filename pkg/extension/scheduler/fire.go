package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	taskext "github.com/hugr-lab/hugen/pkg/extension/task"
	"github.com/hugr-lab/hugen/pkg/protocol"
	tplpkg "github.com/hugr-lab/hugen/pkg/runtime/template"
	"github.com/hugr-lab/hugen/pkg/scheduler/runner"
	schedstore "github.com/hugr-lab/hugen/pkg/scheduler/store"
	"github.com/hugr-lab/hugen/pkg/session"
	"github.com/hugr-lab/hugen/pkg/skill"
)

// SessionHost is the narrow surface the fire dispatch path needs
// from the runtime's session supervisor. Production wiring binds
// *pkg/session/manager.Manager; tests inject a fake. Defining the
// interface inside the scheduler package keeps pkg/extension/scheduler
// from depending on the concrete supervisor.
//
// Phase 6.1c models cron-spawn fires as subagents of the owner root
// session — the spawn itself goes through `owner.Spawn(...)`, NOT
// through this interface. SessionHost is reduced to two operations:
// finding the owner (Get) and pushing wake-kind UserMessages
// (Deliver). The reduction is load-bearing: by reusing
// `parent.Spawn` we inherit subagent_pump (drain), natural
// teardown→SubagentResult termination, and parent-history
// projection — all the plumbing the cron-as-root variant would
// otherwise have to reinvent.
type SessionHost interface {
	// Deliver pushes a frame onto an existing live root session's
	// inbox. Phase 6.1c uses it for `wake` fires — synthesising a
	// UserMessage into the owning session.
	Deliver(ctx context.Context, to string, f protocol.Frame) error

	// Get looks up a live session by id. Returns (nil, false) for
	// terminated / unknown ids. Spawn fires resolve the owner this
	// way, then call owner.Spawn directly; wake fires use this to
	// short-circuit dispatch when the owner session is gone (the
	// task auto-pauses on next-fire scheduling).
	Get(id string) (*session.Session, bool)
}

// buildFireFn returns the RunnerFn the scheduler registers for one
// task. The fn closes over `task`, `store`, `host`, `skills`,
// `agentID`, `logger`; the surrounding extension passes them in
// once at bootstrap. Returned fn is goroutine-safe — runner.Service
// guarantees no concurrent fires of the same registration, so the
// fn relies on Runner serialisation rather than its own mutex.
//
// Per-fire layout (spawn kind):
//
//  1. Drift hash recompute (skill manifest changed since task-create?).
//     Mismatch → AppendLog(skipped, reason=skill_changed), PauseTask,
//     notify owner, return without firing.
//  2. AppendLog(started, planned_at=fire.PlannedAt).
//  3. Build FireContext (LatestSuccessfulFire as PrevFire, etc).
//  4. host.Open(req{Cron: ctx}) → fresh cron session.
//  5. Render the goal template; Deliver a UserMessage that kicks
//     the model loop.
//  6. Wait for the session to terminate via <-sess.Done().
//  7. AppendLog(completed | failed) with last assistant text as
//     outcome.body.
//  8. Compute next planned via the schedule; AppendLog(planned)
//     for the next fire. Recurring tasks (interval / cron)
//     re-register a fresh Schedule via the captured runner
//     reference; one-shot kinds (once_in / once_at) complete.
//  9. Emit scheduler:notification ExtensionFrame on the owner
//     session so the operator sees a status line.
//
// Wake kind path (steps 1, 3 skip):
//   - AppendLog(started)
//   - Render WakeMessage; host.Deliver UserMessage to owner session.
//   - AppendLog(completed) immediately (wake fires don't wait).
//   - Schedule next planned; emit notification.
//
// Returning an error from the fn surfaces as runner-side failure
// (run-log status=failed) AND a task_log failed row. Either signal
// alone is enough; both are written so the runner audit trail +
// task_log stay consistent.
func buildFireFn(task schedstore.TaskRow, deps fireDeps) runner.RunnerFn {
	return func(ctx context.Context, fire runner.FireMeta) (runner.Outcome, error) {
		switch task.Kind {
		case schedstore.KindWake:
			return dispatchWakeFire(ctx, task, fire, deps)
		case schedstore.KindSpawn:
			return dispatchSpawnFire(ctx, task, fire, deps)
		default:
			err := fmt.Errorf("unknown task kind %q", task.Kind)
			appendLogSafely(ctx, deps, terminalLog(task, fire, schedstore.LogEventFailed, &schedstore.TaskOutcome{
				ErrorMessage: err.Error(),
			}))
			return runner.Outcome{ErrorMessage: err.Error()}, err
		}
	}
}

// fireDeps bundles the references buildFireFn captures so the
// factory signature stays compact and adding a new dep (Phase 6.2
// task:set_state writer) doesn't churn every call site.
type fireDeps struct {
	store        schedstore.TaskStore
	host         SessionHost
	skills       *skill.SkillManager
	agentID      string
	logger       *slog.Logger
	rescheduleFn func(taskID string, at time.Time) error
	pauseFn      func(taskID string) error

	// runRecipe is the shared execute helper (task ext's RunRecipe) the
	// spawn-fire path delegates spawn → kick → wait to. Nil until
	// BindRunRecipe wires it; dispatchSpawnFire fails fast when unset.
	runRecipe func(context.Context, taskext.RunParams) (taskext.RunResult, error)

	// stashFire / releaseFire are the per-fire FireContext
	// rendezvous between dispatchSpawnFire (writer) and
	// scheduler.ApplyOnSubagentSpawn (reader). The applier looks up
	// by sanitised SpawnSpec.Name; spawn-name uniqueness is
	// guaranteed by takeSpawnToken (monotonic per-extension counter).
	stashFire      func(spawnName string, fc *protocol.FireContext)
	releaseFire    func(spawnName string)
	takeSpawnToken func() int64
}

// dispatchWakeFire delivers a synthetic UserMessage into the
// owning session and stamps planned-completed-planned bookkeeping
// on task_log. Wake fires never spawn a new session — they nudge
// the owner directly so the model sees a fresh user-role turn the
// next time it's resumed.
func dispatchWakeFire(ctx context.Context, task schedstore.TaskRow, fire runner.FireMeta, deps fireDeps) (runner.Outcome, error) {
	if _, alive := deps.host.Get(task.OwnerSessionID); !alive {
		// Owner session terminated since the task was created —
		// pause the task; a subsequent reattach can resume / cancel.
		deps.pauseTaskLogged(task.ID, schedstore.PauseOwnerTerminated)
		if deps.pauseFn != nil {
			_ = deps.pauseFn(task.ID)
		}
		appendLogSafely(ctx, deps, terminalLog(task, fire, schedstore.LogEventSkipped, &schedstore.TaskOutcome{
			Reason:  schedstore.PauseOwnerTerminated,
			Summary: "owner session terminated",
		}))
		return runner.Outcome{Reason: schedstore.PauseOwnerTerminated}, nil
	}

	appendLogSafely(ctx, deps, schedstore.TaskLogEntry{
		TaskID:    task.ID,
		AgentID:   deps.agentID,
		FireSeq:   fire.FireSeq,
		EventType: schedstore.LogEventStarted,
		PlannedAt: fire.PlannedAt,
	})

	rc := tplpkg.NewFireRenderContext(&protocol.FireContext{
		TaskID:    task.ID,
		FireSeq:   fire.FireSeq,
		PlannedAt: fire.PlannedAt,
		Goal:      task.Spec.Goal,
		Inputs:    task.Spec.Inputs,
		PrevFire:  prevFireFromLog(ctx, deps.store, task.ID, deps.logger),
	})
	// Render per-fire template vars embedded in input values BEFORE the
	// WakeMessage render, so the WakeMessage's {{.Inputs.x}} references
	// see the substituted values (Phase 6 §D7). A bad input template is
	// a render failure — same auto-pause path as a bad WakeMessage.
	var rendered string
	renderedInputs, err := tplpkg.RenderInputs(task.Spec.Inputs, rc)
	if err == nil {
		rc.Inputs = renderedInputs
		rendered, err = tplpkg.RenderTemplate(task.Spec.WakeMessage, rc)
	}
	if err != nil {
		// Render failure auto-pauses the task in the store AND the
		// runner — without the runner pause the broken template
		// would keep re-firing on every tick.
		deps.pauseTaskLogged(task.ID, schedstore.PauseRenderFailed)
		if deps.pauseFn != nil {
			_ = deps.pauseFn(task.ID)
		}
		appendLogSafely(ctx, deps, terminalLog(task, fire, schedstore.LogEventFailed, &schedstore.TaskOutcome{
			ErrorMessage: err.Error(),
			Reason:       schedstore.PauseRenderFailed,
		}))
		return runner.Outcome{ErrorMessage: err.Error()}, err
	}

	frame := protocol.NewUserMessage(task.OwnerSessionID, schedulerParticipant(deps.agentID), rendered)
	if err := deps.host.Deliver(ctx, task.OwnerSessionID, frame); err != nil {
		appendLogSafely(ctx, deps, terminalLog(task, fire, schedstore.LogEventFailed, &schedstore.TaskOutcome{
			ErrorMessage: err.Error(),
			Reason:       "deliver_failed",
		}))
		// Advance the schedule despite the delivery failure — a transient
		// inbox error must not stall a recurring wake.
		maybeScheduleNext(ctx, task, fire, deps)
		return runner.Outcome{ErrorMessage: err.Error()}, err
	}

	appendLogSafely(ctx, deps, terminalLog(task, fire, schedstore.LogEventCompleted, &schedstore.TaskOutcome{
		Summary: fmt.Sprintf("wake delivered (%d chars)", len(rendered)),
	}))
	maybeScheduleNext(ctx, task, fire, deps)
	return runner.Outcome{Summary: "wake delivered"}, nil
}

// dispatchSpawnFire spawns a cron-fire subagent under the task's
// owner root session, kicks the model loop with the rendered goal,
// waits for the subagent to terminate naturally (close_turn ⇒
// SessionClose ⇒ teardown emits SubagentResult into the parent
// pipeline), and folds the outcome into task_log. Drift-pause runs
// FIRST so a renamed-out-from-under-us skill doesn't spawn against
// a stale manifest.
//
// Subagent semantics (vs. the prior cron-as-root design):
//
//   - Drain: subagent_pump in pkg/session already reads child.Outbox
//     and projects relevant frames into the parent. We don't have to
//     spin a separate drainer.
//   - Termination: the subagent emits SubagentResult during teardown
//     once close_turn fires (model-emits-final-response path) — the
//     parent handles it via its normal subagent flow and projects
//     the body into history. No scheduler-specific termination
//     primitive is needed.
//   - Visibility: the SubagentResult lands in the owner's history,
//     so the model sees the cron output on its next turn naturally
//     (this replaces the explicit `scheduler:notification` frame).
func dispatchSpawnFire(ctx context.Context, task schedstore.TaskRow, fire runner.FireMeta, deps fireDeps) (runner.Outcome, error) {
	if drift, err := detectSkillDrift(ctx, deps.skills, task); err != nil {
		appendLogSafely(ctx, deps, terminalLog(task, fire, schedstore.LogEventFailed, &schedstore.TaskOutcome{
			ErrorMessage: err.Error(),
			Reason:       "drift_check_failed",
		}))
		return runner.Outcome{ErrorMessage: err.Error()}, err
	} else if drift != "" {
		// Skill removed / renamed since task-create. Pause + skip.
		deps.pauseTaskLogged(task.ID, drift)
		if deps.pauseFn != nil {
			_ = deps.pauseFn(task.ID)
		}
		appendLogSafely(ctx, deps, terminalLog(task, fire, schedstore.LogEventSkipped, &schedstore.TaskOutcome{
			Reason:  drift,
			Summary: "paused on skill drift",
		}))
		return runner.Outcome{Reason: drift}, nil
	}

	owner, alive := deps.host.Get(task.OwnerSessionID)
	if !alive || owner == nil {
		deps.pauseTaskLogged(task.ID, schedstore.PauseOwnerTerminated)
		if deps.pauseFn != nil {
			_ = deps.pauseFn(task.ID)
		}
		appendLogSafely(ctx, deps, terminalLog(task, fire, schedstore.LogEventSkipped, &schedstore.TaskOutcome{
			Reason:  schedstore.PauseOwnerTerminated,
			Summary: "owner session terminated",
		}))
		return runner.Outcome{Reason: schedstore.PauseOwnerTerminated}, nil
	}

	appendLogSafely(ctx, deps, schedstore.TaskLogEntry{
		TaskID:    task.ID,
		AgentID:   deps.agentID,
		FireSeq:   fire.FireSeq,
		EventType: schedstore.LogEventStarted,
		PlannedAt: fire.PlannedAt,
	})

	prev := prevFireFromLog(ctx, deps.store, task.ID, deps.logger)
	fireCtx := &protocol.FireContext{
		TaskID:       task.ID,
		FireSeq:      fire.FireSeq,
		PlannedAt:    fire.PlannedAt,
		Goal:         task.Spec.Goal,
		Inputs:       task.Spec.Inputs,
		PrevFire:     prev,
		AllowedTools: task.Spec.AllowedTools,
	}

	// Render per-fire template vars embedded in input values (Phase 6
	// §D7): an input like output_path: "report_{{.FireSeq}}.html" must
	// produce a distinct path per fire. The rendered inputs feed BOTH
	// the goal render's {{.Inputs.x}} references and the spawned child's
	// [Inputs from caller] channel. A render FAILURE is NOT recoverable
	// here (unlike the goal, where a literal still kicks the worker):
	// degrading to the literal would ship `{{...}}` into the inputs and
	// defeat D7's whole purpose (every fire writes the same path). A bad
	// input template breaks every fire, so auto-pause — same posture as
	// the wake path's render-failure handling.
	rc := tplpkg.NewFireRenderContext(fireCtx)
	spawnInputs, ierr := tplpkg.RenderInputs(task.Spec.Inputs, rc)
	if ierr != nil {
		deps.pauseTaskLogged(task.ID, schedstore.PauseRenderFailed)
		if deps.pauseFn != nil {
			_ = deps.pauseFn(task.ID)
		}
		appendLogSafely(ctx, deps, terminalLog(task, fire, schedstore.LogEventFailed, &schedstore.TaskOutcome{
			ErrorMessage: ierr.Error(),
			Reason:       schedstore.PauseRenderFailed,
		}))
		return runner.Outcome{ErrorMessage: ierr.Error()}, ierr
	}
	fireCtx.Inputs = spawnInputs
	rc.Inputs = spawnInputs

	// Render the goal as the spawn task body. Render failure is
	// recoverable: we degrade to the literal goal so the subagent
	// still has a kick.
	renderedGoal, err := tplpkg.RenderTemplate(task.Spec.Goal, rc)
	if err != nil {
		deps.logger.Warn("scheduler: goal render failed; using literal",
			"task_id", task.ID, "fire_seq", fire.FireSeq, "err", err)
		renderedGoal = task.Spec.Goal
	}

	// The spawn-fire delegates spawn → scope → pre-load → KICK → wait to
	// the shared task-ext helper, so the cron child runs through the
	// exact same path as an ad-hoc `task:<recipe>` launch. RunRecipe (or
	// the skill manager) must be wired; a fire that lands before
	// BindRunRecipe fails fast rather than spawning a child it can never
	// kick.
	if deps.runRecipe == nil || deps.skills == nil {
		err := fmt.Errorf("scheduler: spawn-fire dispatched before RunRecipe/skills wiring")
		appendLogSafely(ctx, deps, terminalLog(task, fire, schedstore.LogEventFailed, &schedstore.TaskOutcome{
			ErrorMessage: err.Error(),
			Reason:       "spawn_unavailable",
		}))
		return runner.Outcome{ErrorMessage: err.Error()}, err
	}

	// Resolve the recipe skill so RunRecipe can scope the child's skill
	// surface to the manifest whitelist + pre-load the recipe body.
	// Drift was already checked above; a Get failure here means the skill
	// vanished in the gap — pause exactly like the drift path (the next
	// fire is NOT scheduled from this path, so without the pause the task
	// would silently stall with no runner-side stop). A resume / restart
	// re-resolves it.
	sk, serr := deps.skills.Get(ctx, task.SkillRef)
	if serr != nil {
		deps.pauseTaskLogged(task.ID, schedstore.PauseSkillChanged)
		if deps.pauseFn != nil {
			_ = deps.pauseFn(task.ID)
		}
		appendLogSafely(ctx, deps, terminalLog(task, fire, schedstore.LogEventSkipped, &schedstore.TaskOutcome{
			ErrorMessage: serr.Error(),
			Reason:       schedstore.PauseSkillChanged,
			Summary:      "paused on skill resolve failure",
		}))
		return runner.Outcome{Reason: schedstore.PauseSkillChanged}, nil
	}

	// Stash FireContext under a spawn-name token so the scheduler's
	// SubagentSpawnApplier can stamp it on the child's state BEFORE
	// the first task UserMessage lands in the child's inbox. RunRecipe
	// spawns under this exact name, so the applier rendezvous holds.
	// Monotonic counter keeps the token unique across concurrent
	// fires; sanitisation by pkg/session preserves the bare alnum +
	// dash payload (well under the 32-char Name cap).
	spawnName := fmt.Sprintf("cron-%d-%d", fire.FireSeq, deps.takeSpawnToken())
	deps.stashFire(spawnName, fireCtx)
	defer deps.releaseFire(spawnName)

	// CountAsUse stays false: a headless cron tick is not a model-driven
	// launch, so it must not feed the recipe-reuse bandit (only chat /
	// mission-worker launches count). The Runner's per-fire timeout
	// (DefaultFireTimeout 30m) caps the wait via ctx; RunRecipe returns
	// a context error on cancellation, which we map to fire_timeout.
	res, rerr := deps.runRecipe(ctx, taskext.RunParams{
		Anchor:    owner,
		Skill:     sk,
		Recipe:    task.SkillRef,
		SpawnName: spawnName,
		TaskBody:  renderedGoal,
		Inputs:    spawnInputs,
		// Pin the recipe child to WORKER tier. Without this it spawns at
		// depth 1 (child of the owner root) and TierFromDepth(1) defaults
		// it to MISSION — so a leaf recipe executor would get mission/PDCA
		// semantics (planner/checker prompt, no leaf execution). Matches
		// the task:<recipe> + execute_task paths.
		Tier: skill.TierWorker,
		// Persist origin metadata on the child row for liveview /
		// audit grouping by source task without rejoining task_log.
		Metadata: map[string]any{
			"cron_task_id":  task.ID,
			"cron_fire_seq": fire.FireSeq,
		},
		CountAsUse: false,
		// A scheduled task is pre-approved by the user scheduling it, and
		// there is no operator at fire time — so blanket auto-approve its
		// tools. Without this every requires_approval call is denied
		// headless and the worker loops without progress (it can't ask).
		AutoApproveTools: true,
		// Tag the result AsyncNotify: the fire spawns into an IDLE owner
		// root, so its terminal SubagentResult must arm the auto-summary
		// turn or the model never surfaces it (the result lands in
		// history but no turn kicks to show the user). Mirrors an async
		// mission's completion.
		RenderMode: protocol.SubagentRenderAsyncNotify,
	})
	if rerr != nil {
		reason := "spawn_failed"
		if errors.Is(rerr, context.Canceled) || errors.Is(rerr, context.DeadlineExceeded) {
			reason = "fire_timeout"
		}
		appendLogSafely(ctx, deps, terminalLog(task, fire, schedstore.LogEventFailed, &schedstore.TaskOutcome{
			ErrorMessage: rerr.Error(),
			Reason:       reason,
		}))
		// A failed fire still advances the schedule: log the failure, then
		// plan the next fire so one bad run (worker error / timeout) does
		// not silently kill a recurring task. count/until still terminate
		// normally (a failed fire is an attempt); the pause-worthy paths
		// (drift / render / owner-gone) returned earlier without reaching
		// here, so they stay stopped.
		maybeScheduleNext(ctx, task, fire, deps)
		return runner.Outcome{ErrorMessage: rerr.Error()}, rerr
	}

	outcome := &schedstore.TaskOutcome{
		Summary: fmt.Sprintf("cron fire %d completed", fire.FireSeq),
	}
	completed := terminalLog(task, fire, schedstore.LogEventCompleted, outcome)
	completed.SessionID = res.ChildID
	appendLogSafely(ctx, deps, completed)
	maybeScheduleNext(ctx, task, fire, deps)
	return runner.Outcome{Summary: outcome.Summary}, nil
}

// detectSkillDrift returns the pause reason name (PauseSkillChanged
// / PauseSchemaChanged) when the source skill's manifest no longer
// matches the hashes captured at task-create time. Returns ("", nil)
// when the row's hash columns are empty (legacy / hand-edited rows)
// — drift can't be assessed without a baseline.
//
// `tasks.spec.hashes.skill` covers the full manifest YAML; the
// inputs_schema hash narrows the comparison to the task block's
// JSON-Schema subset so a benign body edit doesn't auto-pause.
func detectSkillDrift(ctx context.Context, skills *skill.SkillManager, task schedstore.TaskRow) (string, error) {
	if task.SkillRef == "" || skills == nil {
		return "", nil
	}
	want := task.Spec.Hashes
	if want.Skill == "" && want.InputsSchema == "" {
		return "", nil
	}
	sk, err := skills.Get(ctx, task.SkillRef)
	if err != nil {
		return schedstore.PauseSkillChanged, nil
	}
	if want.Skill != "" {
		got := hashSkillManifest(sk.Manifest)
		if got != want.Skill {
			return schedstore.PauseSkillChanged, nil
		}
	}
	if want.InputsSchema != "" {
		got := hashJSON(sk.Manifest.Hugen.Task.InputsSchema)
		if got != want.InputsSchema {
			return schedstore.PauseSchemaChanged, nil
		}
	}
	return "", nil
}

// hashSkillManifest computes a stable sha256 over the manifest's
// frontmatter + body. Used by drift detection at fire time and (in
// 6.2) by schedule:create to populate `tasks.spec.hashes.skill`.
func hashSkillManifest(m skill.Manifest) string {
	h := sha256.New()
	h.Write(m.Raw)
	h.Write(m.Body)
	return hex.EncodeToString(h.Sum(nil))
}

// hashJSON computes a stable sha256 over a JSON-serialisable value
// with sorted-keys serialisation so map ordering doesn't perturb
// the digest.
func hashJSON(v any) string {
	if v == nil {
		return ""
	}
	body, err := stableJSONMarshal(v)
	if err != nil {
		return ""
	}
	h := sha256.New()
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// stableJSONMarshal serialises v with sorted map keys so the same
// logical value always produces the same byte stream — needed for
// hash-equality across processes.
func stableJSONMarshal(v any) ([]byte, error) {
	return stableMarshalValue(v)
}

func stableMarshalValue(v any) ([]byte, error) {
	switch val := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var buf []byte
		buf = append(buf, '{')
		for i, k := range keys {
			if i > 0 {
				buf = append(buf, ',')
			}
			kb, _ := json.Marshal(k)
			buf = append(buf, kb...)
			buf = append(buf, ':')
			child, err := stableMarshalValue(val[k])
			if err != nil {
				return nil, err
			}
			buf = append(buf, child...)
		}
		buf = append(buf, '}')
		return buf, nil
	case []any:
		var buf []byte
		buf = append(buf, '[')
		for i, item := range val {
			if i > 0 {
				buf = append(buf, ',')
			}
			child, err := stableMarshalValue(item)
			if err != nil {
				return nil, err
			}
			buf = append(buf, child...)
		}
		buf = append(buf, ']')
		return buf, nil
	default:
		return json.Marshal(v)
	}
}

// prevFireFromLog reads the LatestSuccessfulFire entry and projects
// its outcome onto a [protocol.PrevFireOutcome] suitable for
// FireRenderContext. Returns nil when no prior completion exists.
func prevFireFromLog(ctx context.Context, st schedstore.TaskStore, taskID string, log *slog.Logger) *protocol.PrevFireOutcome {
	entry, err := st.LatestSuccessfulFire(ctx, taskID)
	if err != nil {
		log.Warn("scheduler: LatestSuccessfulFire failed",
			"task_id", taskID, "err", err)
		return nil
	}
	if entry == nil {
		return nil
	}
	out := &protocol.PrevFireOutcome{
		FiredAt:   entry.PlannedAt,
		SessionID: entry.SessionID,
	}
	if entry.Outcome != nil {
		out.Summary = entry.Outcome.Summary
		out.Body = entry.Outcome.Body
		out.State = entry.Outcome.StateDiff
	}
	return out
}

// terminalLog builds a TaskLogEntry for the terminal event of a
// fire. Caller fills SessionID / extra fields if relevant.
func terminalLog(task schedstore.TaskRow, fire runner.FireMeta, eventType string, outcome *schedstore.TaskOutcome) schedstore.TaskLogEntry {
	return schedstore.TaskLogEntry{
		TaskID:    task.ID,
		AgentID:   task.AgentID,
		FireSeq:   fire.FireSeq,
		EventType: eventType,
		PlannedAt: fire.PlannedAt,
		Outcome:   outcome,
	}
}

// maybeScheduleNext computes the next planned instant from the task's
// schedule, inserts the corresponding `planned` row, and reschedules
// the runner registration's next fire to it (in place — no
// re-registration, so the fire counter stays monotonic). End-condition
// check (count exceeded / until passed) short-circuits with
// status='cancelled' on the task row instead.
//
// The next instant is the LOGICAL one (prevPlanned + interval / the
// next cron tick), written verbatim — no clamp. If a long fire pushed
// it into the past, Reschedule still arms it and the runner fires it on
// the next tick (nextFireAt <= now), so a task slower than its interval
// runs its full count back-to-back. One-shot kinds (once_in / once_at)
// skip the next planned row and cancel the task.
func maybeScheduleNext(ctx context.Context, task schedstore.TaskRow, fire runner.FireMeta, deps fireDeps) {
	switch task.ScheduleKind {
	case schedstore.ScheduleOnceIn, schedstore.ScheduleOnceAt:
		// One-shot — mark the task completed so ListDue stops
		// returning it.
		if err := deps.store.CancelTask(context.Background(), task.ID); err != nil {
			deps.logger.Warn("scheduler: mark one-shot completed",
				"task_id", task.ID, "err", err)
		}
		return
	case schedstore.ScheduleInterval:
		d, err := time.ParseDuration(task.Spec.ScheduleSpec)
		if err != nil || d <= 0 {
			deps.logger.Warn("scheduler: invalid interval; pausing task",
				"task_id", task.ID, "spec", task.Spec.ScheduleSpec, "err", err)
			deps.pauseTaskLogged(task.ID, schedstore.PauseRenderFailed)
			if deps.pauseFn != nil {
				_ = deps.pauseFn(task.ID)
			}
			return
		}
		next := fire.PlannedAt.Add(d).UTC()
		if shouldEndAfter(task.Spec.EndCondition, fire.FireSeq, next) {
			if err := deps.store.CancelTask(context.Background(), task.ID); err != nil {
				deps.logger.Warn("scheduler: end-condition cancel",
					"task_id", task.ID, "err", err)
			}
			return
		}
		appendLogSafely(ctx, deps, schedstore.TaskLogEntry{
			TaskID:    task.ID,
			AgentID:   deps.agentID,
			FireSeq:   fire.FireSeq + 1,
			EventType: schedstore.LogEventPlanned,
			PlannedAt: next,
		})
		if deps.rescheduleFn != nil {
			if err := deps.rescheduleFn(task.ID, next); err != nil {
				deps.logger.Warn("scheduler: reschedule interval task",
					"task_id", task.ID, "err", err)
			}
		}
	case schedstore.ScheduleCron:
		loc, err := resolveLocation(task.Spec.Timezone)
		if err != nil {
			deps.logger.Warn("scheduler: invalid cron timezone; pausing task",
				"task_id", task.ID, "timezone", task.Spec.Timezone, "err", err)
			deps.pauseTaskLogged(task.ID, schedstore.PauseRenderFailed)
			if deps.pauseFn != nil {
				_ = deps.pauseFn(task.ID)
			}
			return
		}
		sched, err := runner.Cron(task.Spec.ScheduleSpec, loc)
		if err != nil {
			deps.logger.Warn("scheduler: invalid cron expression; pausing task",
				"task_id", task.ID, "spec", task.Spec.ScheduleSpec, "err", err)
			deps.pauseTaskLogged(task.ID, schedstore.PauseRenderFailed)
			if deps.pauseFn != nil {
				_ = deps.pauseFn(task.ID)
			}
			return
		}
		next := sched.Next(fire.PlannedAt)
		if next.IsZero() {
			// No further fire (a cron that can never match again).
			if err := deps.store.CancelTask(context.Background(), task.ID); err != nil {
				deps.logger.Warn("scheduler: cron exhausted cancel",
					"task_id", task.ID, "err", err)
			}
			return
		}
		if shouldEndAfter(task.Spec.EndCondition, fire.FireSeq, next) {
			if err := deps.store.CancelTask(context.Background(), task.ID); err != nil {
				deps.logger.Warn("scheduler: end-condition cancel",
					"task_id", task.ID, "err", err)
			}
			return
		}
		appendLogSafely(ctx, deps, schedstore.TaskLogEntry{
			TaskID:    task.ID,
			AgentID:   deps.agentID,
			FireSeq:   fire.FireSeq + 1,
			EventType: schedstore.LogEventPlanned,
			PlannedAt: next,
		})
		if deps.rescheduleFn != nil {
			if err := deps.rescheduleFn(task.ID, next); err != nil {
				deps.logger.Warn("scheduler: reschedule cron task",
					"task_id", task.ID, "err", err)
			}
		}
	default:
		deps.logger.Warn("scheduler: unknown schedule_kind; pausing",
			"task_id", task.ID, "schedule_kind", task.ScheduleKind)
		deps.pauseTaskLogged(task.ID, schedstore.PauseRenderFailed)
		if deps.pauseFn != nil {
			_ = deps.pauseFn(task.ID)
		}
	}
}

// shouldEndAfter reports whether the end condition has been reached
// at fire `seq` planned for `next`. Used by maybeScheduleNext to
// short-circuit one further fire.
func shouldEndAfter(end schedstore.TaskEndCondition, completedSeq int, nextAt time.Time) bool {
	switch end.Kind {
	case "", "until_cancel":
		return false
	case "count":
		n, err := parseIntStrict(end.Spec)
		if err != nil || n <= 0 {
			return false
		}
		return completedSeq >= n
	case "until":
		at, err := time.Parse(time.RFC3339, end.Spec)
		if err != nil {
			return false
		}
		return !nextAt.Before(at)
	default:
		return false
	}
}

func parseIntStrict(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, errors.New("empty")
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("non-digit %q at %d", c, i)
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

// appendLogSafely wraps a single store.AppendLog with a background
// ctx so a cancelled fire still persists its terminal event. Errors
// are logged at warn; the audit trail is best-effort but the
// non-cancelled retry pipeline catches genuine store outages.
func appendLogSafely(_ context.Context, deps fireDeps, entry schedstore.TaskLogEntry) {
	if err := deps.store.AppendLog(context.Background(), entry); err != nil {
		deps.logger.Warn("scheduler: AppendLog failed",
			"task_id", entry.TaskID,
			"event_type", entry.EventType,
			"fire_seq", entry.FireSeq,
			"err", err)
	}
}

// pauseTaskLogged pauses a task, logging a store failure at WARN instead of
// swallowing it — a silent PauseTask leaves the task un-paused with no trace.
func (d fireDeps) pauseTaskLogged(taskID string, reason string) {
	if err := d.store.PauseTask(context.Background(), taskID, reason); err != nil && d.logger != nil {
		d.logger.Warn("scheduler: pause task failed", "task", taskID, "reason", reason, "err", err)
	}
}
