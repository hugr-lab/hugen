// Package store is the persistence facade for the Phase 6 cron +
// scheduler subsystem. It owns two tables under `hub.db.agent`:
//
//   - `tasks` — one row per user-defined schedule entry. Lifecycle
//     UPDATEs only (pause / resume / cancel / spec edit) — 0-2
//     writes per task lifetime. Never per fire.
//   - `task_log` — pure append-only event log: every fire emits
//     >= 2 rows (`planned` then terminal `completed` / `failed`),
//     plus optional intermediate `started` / `log` rows.
//     Symmetric with `session_events` / `memory_log`.
//
// All per-fire writes go through [TaskStore.AppendLog]. The "next
// planned fire" / "last successful fire" / "stuck fire" queries are
// derived from `task_log` via the `(task_id, event_type, fire_seq
// DESC, created_at DESC)` index — no joins to `tasks`, no
// aggregation. See design/004-runtime-post-phase-i/phase-6-spec.md
// §2.4.1 for canonical query patterns.
package store

import (
	"context"
	"errors"
	"time"
)

// Sentinel errors returned by [TaskStore] implementations.
var (
	ErrTaskNotFound  = errors.New("scheduler/store: task not found")
	ErrTaskDuplicate = errors.New("scheduler/store: task already exists")
	ErrInvalidStatus = errors.New("scheduler/store: invalid task status")
	ErrInvalidLog    = errors.New("scheduler/store: invalid task_log entry")
)

// TaskKind classifies how a fire is delivered.
//   - "wake" — synthetic UserMessage into the owner session (Type A
//     / B1 from spec §1.1).
//   - "spawn" — a fresh cron session per fire (Type B2 / B3).
const (
	KindWake  = "wake"
	KindSpawn = "spawn"
)

// TaskStatus is the task lifecycle column. Runner ticks reconcile
// by `status = "active"`; UPDATEs are narrow per action and never
// emitted on the steady-state fire path.
const (
	StatusActive    = "active"
	StatusPaused    = "paused"
	StatusCancelled = "cancelled"
	StatusCompleted = "completed"
)

// ScheduleKind discriminates the shape of [TaskSpec.ScheduleSpec].
// Concrete parsing per-kind lives in the TaskManager extension; the
// store treats `schedule_spec` as opaque text.
const (
	ScheduleOnceIn   = "once_in"
	ScheduleOnceAt   = "once_at"
	ScheduleCron     = "cron"
	ScheduleInterval = "interval"
)

// LogEventType discriminates [TaskLogEntry] rows. Spec §2.4 enumerates
// the required vs optional payload fields per event_type.
const (
	LogEventPlanned   = "planned"
	LogEventStarted   = "started"
	LogEventLog       = "log"
	LogEventCompleted = "completed"
	LogEventFailed    = "failed"
	LogEventSkipped   = "skipped"
	LogEventCancelled = "cancelled"
)

// PauseReason is the `tasks.pause_reason` column written atomically
// alongside `status = "paused"`. NULL while active.
const (
	PauseUser             = "user"
	PauseSkillChanged     = "skill_changed"
	PauseSchemaChanged    = "schema_changed"
	PauseRenderFailed     = "render_failed"
	PauseOwnerTerminated  = "owner_terminated"
)

// TaskRow mirrors the hub.db.agent.tasks row layout. The free-form
// `spec` JSON column projects onto [TaskSpec]; the other columns are
// either filter / index columns or narrow lifecycle state.
//
// Per-fire UPDATEs to this row are zero: the schedule anchor (next
// planned fire) lives in `task_log` as a `planned` row inserted by
// [TaskStore.OpenTask] (fire_seq=1) atomically with the row itself.
type TaskRow struct {
	ID             string    `json:"id"`
	AgentID        string    `json:"agent_id"`
	Kind           string    `json:"kind"`
	Status         string    `json:"status"`
	ScheduleKind   string    `json:"schedule_kind"`
	OwnerSessionID string    `json:"owner_session_id"`
	SkillRef       string    `json:"skill_ref,omitempty"`
	Spec           TaskSpec  `json:"spec"`
	PauseReason    string    `json:"pause_reason,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// TaskSpec is the typed projection of the `tasks.spec` JSON column.
// Stored as JSON; exposed via the GraphQL `TaskSpec` object type.
// Fields with skill-specific shape (Inputs) are free-form maps.
type TaskSpec struct {
	Name          string           `json:"name"`
	Description   string           `json:"description,omitempty"`
	ScheduleSpec  string           `json:"schedule_spec"`
	EndCondition  TaskEndCondition `json:"end_condition"`
	Goal          string           `json:"goal,omitempty"`
	WakeMessage   string           `json:"wake_message,omitempty"`
	AllowedTools  []string         `json:"allowed_tools,omitempty"`
	Inputs        map[string]any   `json:"inputs,omitempty"`
	Hashes        TaskHashes       `json:"hashes"`
}

// TaskEndCondition declares when a recurring task should stop firing.
// Kind: "until_cancel" | "count" | "until". Spec holds count int or
// RFC3339 timestamp encoded as string; NULL when kind=="until_cancel".
type TaskEndCondition struct {
	Kind string `json:"kind"`
	Spec string `json:"spec,omitempty"`
}

// TaskHashes carries skill / inputs_schema / inputs sha256 fingerprints
// captured at task-create time. TaskManager re-hashes at fire time
// and pauses on drift (skill_changed / schema_changed). The inputs
// hash is audit-only — does not trigger pause.
type TaskHashes struct {
	Skill        string `json:"skill,omitempty"`
	InputsSchema string `json:"inputs_schema,omitempty"`
	Inputs       string `json:"inputs,omitempty"`
}

// TaskLogEntry mirrors one hub.db.agent.task_log row. Append-only —
// the store never UPDATEs an entry. `FireSeq` + `PlannedAt` are
// denormalised on every row of the fire so latest-by-fire reads
// skip joins.
type TaskLogEntry struct {
	ID        string       `json:"id"`
	TaskID    string       `json:"task_id"`
	AgentID   string       `json:"agent_id"`
	FireSeq   int          `json:"fire_seq"`
	EventType string       `json:"event_type"`
	PlannedAt time.Time    `json:"planned_at"`
	SessionID string       `json:"session_id,omitempty"`
	Outcome   *TaskOutcome `json:"outcome,omitempty"`
	Content   string       `json:"content,omitempty"`
	CreatedAt time.Time    `json:"created_at"`
}

// TaskOutcome is the body of `task_log.outcome` on terminal events
// (completed / failed / skipped / cancelled). Failed fires leave
// StateDiff nil so [TaskStore.LatestSuccessfulFire] keeps returning
// the prior successful run.
type TaskOutcome struct {
	Summary      string         `json:"summary,omitempty"`
	Body         string         `json:"body,omitempty"`
	StateDiff    map[string]any `json:"state_diff,omitempty"`
	Usage        map[string]any `json:"usage,omitempty"`
	ErrorMessage string         `json:"error_message,omitempty"`
	Reason       string         `json:"reason,omitempty"`
}

// ListTasksOpts narrows the result set returned by
// [TaskStore.ListTasksBySession]. Empty Status returns rows of every
// status. Limit <= 0 means "use the implementation default".
type ListTasksOpts struct {
	Status string
	Limit  int
}

// ListLogOpts narrows the result set returned by
// [TaskStore.ListLogByTask]. EventTypes is an `event_type IN (…)`
// filter; empty means all kinds. SinceFireSeq (when > 0) returns
// rows with `fire_seq >= SinceFireSeq`. Limit <= 0 means "use the
// implementation default".
type ListLogOpts struct {
	EventTypes   []string
	SinceFireSeq int
	Limit        int
}

// TaskStore is the persistence facade consumed by TaskManager. Narrow
// lifecycle UPDATEs per action — no catch-all `UpdateTask(patch)` —
// so callers can't accidentally widen the UPDATE set. All `task_log`
// writes go through [TaskStore.AppendLog] (pure INSERT).
type TaskStore interface {
	// OpenTask inserts the tasks row AND the initial `task_log`
	// `planned` row (fire_seq=1, planned_at=initialPlanned)
	// atomically. After this call, [TaskStore.ListDue] returns the
	// task once initialPlanned <= now. Returns [ErrTaskDuplicate] if
	// a row with the same ID already exists.
	OpenTask(ctx context.Context, row TaskRow, initialPlanned time.Time) error

	GetTask(ctx context.Context, id string) (TaskRow, error)
	ListTasksBySession(ctx context.Context, sessionID string, opts ListTasksOpts) ([]TaskRow, error)

	// ListDue returns active tasks for agentID whose latest
	// `task_log` `planned` row has `planned_at <= now`. Limit caps
	// the result set (<= 0 = implementation default). Backed by the
	// `(task_id, event_type, fire_seq DESC, created_at DESC)` index.
	ListDue(ctx context.Context, agentID string, now time.Time, limit int) ([]TaskRow, error)

	DeleteTask(ctx context.Context, id string) error

	// PauseTask sets status='paused' and pause_reason=reason in one
	// targeted UPDATE. Reason is one of the [PauseUser]/...
	// constants — the store does not validate this set; callers
	// (TaskManager) own the semantics.
	PauseTask(ctx context.Context, id, reason string) error

	// ResumeTask sets status='active' and clears pause_reason in one
	// targeted UPDATE. Caller (TaskManager) is responsible for any
	// hash re-verification before resuming.
	ResumeTask(ctx context.Context, id string) error

	// CancelTask sets status='cancelled' in one targeted UPDATE.
	CancelTask(ctx context.Context, id string) error

	// UpdateTaskSpec replaces the `spec` JSON blob in one targeted
	// UPDATE. Called only on user-initiated edit (rare). Does NOT
	// touch `task_log`; if the schedule changed, the caller should
	// also append a new `planned` row.
	UpdateTaskSpec(ctx context.Context, id string, spec TaskSpec) error

	// AppendLog is the SOLE write path for `task_log`. Pure INSERT —
	// no UPDATE counterpart. Caller fills EventType / FireSeq /
	// PlannedAt + payload fields per spec §2.4. The store may
	// generate ID / CreatedAt when left zero.
	AppendLog(ctx context.Context, entry TaskLogEntry) error

	// ListLogByTask returns log entries for taskID in (fire_seq
	// DESC, created_at DESC) order — newest fire first, newest
	// event within a fire first. Opts narrow by event_type / fire_seq
	// floor / row count.
	ListLogByTask(ctx context.Context, taskID string, opts ListLogOpts) ([]TaskLogEntry, error)

	// LatestPlannedFire returns the most recent `planned` row for
	// taskID, or (nil, nil) if none. This is the schedule anchor:
	// [ListDue] resolves the same row via the indexed subquery.
	LatestPlannedFire(ctx context.Context, taskID string) (*TaskLogEntry, error)

	// LatestSuccessfulFire returns the most recent `completed` row
	// for taskID — the `.PrevFire` source for cron-spawned template
	// rendering. Returns (nil, nil) if the task has never completed
	// a fire.
	LatestSuccessfulFire(ctx context.Context, taskID string) (*TaskLogEntry, error)

	// ListInFlightFires returns `started` rows that have no matching
	// terminal row (completed / failed / cancelled / skipped) for the
	// same (task_id, fire_seq) AND whose started.created_at <
	// startedBefore. Drives `task_log_reap_stuck` (§16.1) — the
	// reaper appends a `cancelled` row with outcome.reason='reap_stuck'
	// for each entry returned.
	ListInFlightFires(ctx context.Context, agentID string, startedBefore time.Time) ([]TaskLogEntry, error)
}
