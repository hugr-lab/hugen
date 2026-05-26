package protocol

import "time"

// SessionKind classifies a session by its origin / lifecycle shape.
// Stored on the hub.db.agent.sessions row's `session_type` column —
// Phase 6 reuses the existing discriminator rather than introducing
// a parallel `kind` column. Values:
//
//   - [SessionKindRoot]     — user-initiated conversation. Default.
//   - [SessionKindSubagent] — agent-spawned specialist with its own
//     isolated context (session_type for the existing subagent shape).
//   - [SessionKindCron]     — scheduler-spawned fire session
//     (Phase 6 §1.1 Type B2 / B3). Filtered out of root-session
//     adapter listings; operator sees only the resulting
//     `scheduler:notification` ExtensionFrame in the owner session.
//   - [SessionKindFork]     — reserved for future user-initiated forks
//     (shared parent history). Pre-existing reservation; unused.
const (
	SessionKindRoot     = "root"
	SessionKindSubagent = "subagent"
	SessionKindCron     = "cron"
	SessionKindFork     = "fork"
)

// FireContext is the Cron-spawn parameter envelope. Set on the
// session.OpenRequest.Cron field by TaskManager when opening a
// scheduler-driven session (Phase 6 §5.2). Nil for all other kinds.
//
// Carries the per-fire data the spawned mission needs: task identity,
// fire sequence, planned instant, the user-declared goal + structured
// inputs, the prior-fire's outcome blob (`.PrevFire` source for
// template rendering — §1.2.4), and the per-task tool allow-list that
// CronApprovalPolicy enforces in 6.1c (§0.5.6). Pre-approval marker
// rides MissionState in 6.2; the field is here from day one so the
// envelope shape stays stable.
type FireContext struct {
	// TaskID is the `tasks.id` of the owning user task. Stamped into
	// task_log + session metadata so the spawned cron session can be
	// traced back to its source row.
	TaskID string

	// FireSeq is the per-task 1-indexed fire counter. Matches the
	// `task_log.fire_seq` value the surrounding `started` /
	// `completed` rows carry.
	FireSeq int

	// PlannedAt is the schedule-output instant Runner targeted for
	// this fire. May lag the real clock by up to one tick interval.
	PlannedAt time.Time

	// Goal is the imperative one-line brief from
	// `tasks.spec.goal`. Surfaces in liveview, notification
	// subjects. Empty for `wake` kind (which uses `WakeMessage`).
	Goal string

	// Inputs is the structured per-skill parameter blob from
	// `tasks.spec.inputs`, validated against the skill's
	// `task.inputs_schema` at task-create time. Free-form map.
	Inputs map[string]any

	// PrevFire is the most recent successful fire's outcome, or nil
	// on the first fire. Used as `.PrevFire` in cron-spawn template
	// rendering (§1.2.4) — populated by TaskManager from
	// `TaskStore.LatestSuccessfulFire` before Open.
	PrevFire *PrevFireOutcome

	// AllowedTools is the per-task tool allow-list pre-approved at
	// task-create time. Empty list means "no tool calls allowed in
	// this fire". Drives CronApprovalPolicy in 6.1c.
	AllowedTools []string
}

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

	// SessionID is the prior cron session id (empty for wake fires).
	SessionID string

	// State carries the prior fire's cumulative `task:set_state`
	// writes (the `outcome.state_diff` JSON). Read via
	// `.PrevFire.State.<key>` in templates. Phase 6.3 wires
	// task:set_state / task:get_state through this map.
	State map[string]any
}
