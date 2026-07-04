package protocol

import "time"

// SessionKind classifies a session by its origin / lifecycle shape.
// Stored on the hub.agent.db.sessions row's `session_type` column.
// Values:
//
//   - [SessionKindRoot]     — user-initiated conversation. Default.
//   - [SessionKindSubagent] — agent-spawned specialist with its own
//     isolated context. Cron fire sessions (Phase 6.1c) are also
//     subagents — they spawn under the task's owner root and emit
//     a SubagentResult into the parent on teardown.
//   - [SessionKindFork]     — reserved for future user-initiated forks
//     (shared parent history). Pre-existing reservation; unused.
const (
	SessionKindRoot     = "root"
	SessionKindSubagent = "subagent"
	SessionKindFork     = "fork"
)

// FireContext is the per-cron-fire envelope the scheduler hands to
// the spawned subagent via [SchedulerFireStateKey]. The envelope
// carries the task identity, the fire counter, the planned instant,
// the user-declared goal + structured inputs, the prior-fire's
// outcome (for `.PrevFire` template references), and the per-task
// tool allow-list that [CronApprovalPolicy] enforces during the
// fire.
//
// The scheduler extension is the sole producer; consumers are the
// cron-system prompt advertiser, the CronApprovalPolicy, and any
// future template-rendering surface that wants per-fire metadata.
type FireContext struct {
	// TaskID is the `tasks.id` of the owning user task. Stamped into
	// task_log + child session metadata so the spawned cron subagent
	// can be traced back to its source row.
	TaskID string

	// FireSeq is the per-task 1-indexed fire counter. Matches the
	// `task_log.fire_seq` value the surrounding `started` /
	// `completed` rows carry.
	FireSeq int

	// PlannedAt is the schedule-output instant Runner targeted for
	// this fire. May lag the real clock by up to one tick interval.
	PlannedAt time.Time

	// Goal is the imperative one-line brief from
	// `tasks.spec.goal`. Empty for `wake` kind (which uses
	// `WakeMessage` instead).
	Goal string

	// Inputs is the structured per-skill parameter blob from
	// `tasks.spec.inputs`. Free-form map; the scheduler propagates
	// it verbatim onto [SpawnSpec.Inputs] so the spawned worker's
	// skill template can interpolate via the standard
	// `[Inputs from caller]` channel.
	Inputs map[string]any

	// PrevFire is the most recent successful fire's outcome, or nil
	// on the first fire. Used as `.PrevFire` in cron-spawn template
	// rendering — populated by TaskManager from
	// `TaskStore.LatestSuccessfulFire` before Spawn.
	PrevFire *PrevFireOutcome

	// AllowedTools is the per-task tool allow-list pre-approved at
	// task-create time. Empty list means "no tool calls allowed in
	// this fire". Drives CronApprovalPolicy.
	AllowedTools []string
}

// SchedulerFireStateKey is the [SessionState.SetValue] key the
// scheduler stamps with a `*FireContext` value on cron-spawned
// subagents (via [SubagentSpawnApplier]). Extensions that need to
// detect "this is a cron fire" (CronApprovalPolicy, the cron-prompt
// advertiser) read it from session state without widening the
// SessionState interface. The constant lives alongside [FireContext]
// so producers (pkg/extension/scheduler) and consumers share one
// canonical key.
const SchedulerFireStateKey = "scheduler.fire"

// PrevFireOutcome is the subset of `task_log{event_type='completed'}.outcome`
// that template rendering may reference via `.PrevFire`. Shape kept
// independent of [store.TaskOutcome] so pkg/protocol can stay free
// of the storage import.
type PrevFireOutcome struct {
	// FiredAt is the planned instant of the prior fire (matches the
	// prior log row's `planned_at`).
	FiredAt time.Time

	// Summary is the prior fire's short outcome summary.
	Summary string

	// Body is the prior fire's full notification body. May be large.
	Body string

	// SessionID is the prior cron subagent id (empty for wake fires).
	SessionID string

	// State carries the prior fire's cumulative `task:set_state`
	// writes (the `outcome.state_diff` JSON). Read via
	// `.PrevFire.State.<key>` in templates. Phase 6.3 wires
	// task:set_state / task:get_state through this map.
	State map[string]any
}
