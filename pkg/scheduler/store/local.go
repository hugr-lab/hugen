package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hugr-lab/query-engine/types"

	"github.com/hugr-lab/hugen/pkg/store/queries"
)

// LocalTaskStore is the GraphQL-backed [TaskStore] implementation —
// runs every operation through the hub.db engine via
// queries.RunQuery / queries.RunMutation. Mirrors the
// pkg/session/store.RuntimeStoreLocal pattern.
//
// Concurrency: stateless; safe to share across goroutines. Each
// method's GraphQL call is independent — the embedded hugr engine
// serialises writes against the underlying DuckDB connection.
type LocalTaskStore struct {
	querier types.Querier
}

// NewLocalTaskStore constructs the local-store facade.
func NewLocalTaskStore(q types.Querier) *LocalTaskStore {
	return &LocalTaskStore{querier: q}
}

// Compile-time check.
var _ TaskStore = (*LocalTaskStore)(nil)

// taskRowProjection is the column set every tasks-read path projects.
// Kept as a constant so multiple call sites stay in sync.
const taskRowProjection = `
	id agent_id kind status schedule_kind owner_session_id
	skill_ref spec pause_reason created_at updated_at
`

// taskLogProjection is the column set every task_log read path projects.
const taskLogProjection = `
	id task_id agent_id fire_seq event_type planned_at
	session_id outcome content created_at
`

// taskRowDB is the wire shape for a `tasks` row returned by Hugr.
// The `spec` column comes back as a JSON string from DuckDB (Arrow
// utf8) and a JSONB map from Postgres — we accept either via string
// scanning here, then JSON-decode into [TaskSpec].
type taskRowDB struct {
	ID             string    `json:"id"`
	AgentID        string    `json:"agent_id"`
	Kind           string    `json:"kind"`
	Status         string    `json:"status"`
	ScheduleKind   string    `json:"schedule_kind"`
	OwnerSessionID string    `json:"owner_session_id"`
	SkillRef       string    `json:"skill_ref,omitempty"`
	Spec           string    `json:"spec"`
	PauseReason    string    `json:"pause_reason,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func (r taskRowDB) toRow() (TaskRow, error) {
	out := TaskRow{
		ID:             r.ID,
		AgentID:        r.AgentID,
		Kind:           r.Kind,
		Status:         r.Status,
		ScheduleKind:   r.ScheduleKind,
		OwnerSessionID: r.OwnerSessionID,
		SkillRef:       r.SkillRef,
		PauseReason:    r.PauseReason,
		CreatedAt:      r.CreatedAt,
		UpdatedAt:      r.UpdatedAt,
	}
	if r.Spec != "" && r.Spec != "null" {
		if err := json.Unmarshal([]byte(r.Spec), &out.Spec); err != nil {
			return TaskRow{}, fmt.Errorf("scheduler/store: decode tasks.spec for %q: %w", r.ID, err)
		}
	}
	return out, nil
}

// taskLogRowDB is the wire shape for a `task_log` row. `outcome`
// comes back as JSON text (DuckDB) or JSONB (Postgres); we accept
// either via string scanning and JSON-decode into [TaskOutcome] when
// present.
type taskLogRowDB struct {
	ID        string    `json:"id"`
	TaskID    string    `json:"task_id"`
	AgentID   string    `json:"agent_id"`
	FireSeq   int       `json:"fire_seq"`
	EventType string    `json:"event_type"`
	PlannedAt time.Time `json:"planned_at"`
	SessionID string    `json:"session_id,omitempty"`
	Outcome   string    `json:"outcome,omitempty"`
	Content   string    `json:"content,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func (r taskLogRowDB) toEntry() (TaskLogEntry, error) {
	out := TaskLogEntry{
		ID:        r.ID,
		TaskID:    r.TaskID,
		AgentID:   r.AgentID,
		FireSeq:   r.FireSeq,
		EventType: r.EventType,
		PlannedAt: r.PlannedAt,
		SessionID: r.SessionID,
		Content:   r.Content,
		CreatedAt: r.CreatedAt,
	}
	if r.Outcome != "" && r.Outcome != "null" {
		var outcome TaskOutcome
		if err := json.Unmarshal([]byte(r.Outcome), &outcome); err != nil {
			return TaskLogEntry{}, fmt.Errorf("scheduler/store: decode task_log.outcome for %q: %w", r.ID, err)
		}
		out.Outcome = &outcome
	}
	return out, nil
}

// OpenTask inserts the tasks row AND the initial task_log `planned`
// row (fire_seq=1, planned_at=initialPlanned). Two GraphQL mutations —
// the storage layer below (hub.db on DuckDB / Postgres) does not
// expose a multi-statement transaction surface through the Querier
// interface, so we issue them sequentially. If the second insert
// fails we attempt a best-effort delete of the tasks row to keep the
// store consistent.
func (s *LocalTaskStore) OpenTask(ctx context.Context, row TaskRow, initialPlanned time.Time) error {
	if row.ID == "" {
		return fmt.Errorf("scheduler/store: OpenTask requires ID")
	}
	if row.AgentID == "" {
		return fmt.Errorf("scheduler/store: OpenTask requires AgentID")
	}
	if row.Kind != KindWake && row.Kind != KindSpawn {
		return fmt.Errorf("scheduler/store: OpenTask: invalid kind %q", row.Kind)
	}
	if row.OwnerSessionID == "" {
		return fmt.Errorf("scheduler/store: OpenTask requires OwnerSessionID")
	}
	if row.ScheduleKind == "" {
		return fmt.Errorf("scheduler/store: OpenTask requires ScheduleKind")
	}
	if initialPlanned.IsZero() {
		return fmt.Errorf("scheduler/store: OpenTask requires non-zero initialPlanned")
	}
	if row.Status == "" {
		row.Status = StatusActive
	}

	specBytes, err := json.Marshal(row.Spec)
	if err != nil {
		return fmt.Errorf("scheduler/store: marshal spec: %w", err)
	}
	var specMap map[string]any
	if err := json.Unmarshal(specBytes, &specMap); err != nil {
		return fmt.Errorf("scheduler/store: re-decode spec: %w", err)
	}

	data := map[string]any{
		"id":               row.ID,
		"agent_id":         row.AgentID,
		"kind":             row.Kind,
		"status":           row.Status,
		"schedule_kind":    row.ScheduleKind,
		"owner_session_id": row.OwnerSessionID,
		"spec":             specMap,
	}
	if row.SkillRef != "" {
		data["skill_ref"] = row.SkillRef
	}
	if row.PauseReason != "" {
		data["pause_reason"] = row.PauseReason
	}

	if err := queries.RunMutation(ctx, s.querier,
		`mutation ($data: hub_agent_db_tasks_mut_input_data!) {
			hub { agent { db {
				insert_tasks(data: $data) { id }
			}}}
		}`,
		map[string]any{"data": data},
	); err != nil {
		return fmt.Errorf("scheduler/store: insert tasks: %w", err)
	}

	// Anchor the schedule in task_log.
	if err := s.AppendLog(ctx, TaskLogEntry{
		TaskID:    row.ID,
		AgentID:   row.AgentID,
		FireSeq:   1,
		EventType: LogEventPlanned,
		PlannedAt: initialPlanned,
	}); err != nil {
		// Best-effort rollback of the tasks row so the table doesn't
		// accumulate orphan tasks without a schedule anchor.
		_ = s.DeleteTask(ctx, row.ID)
		return fmt.Errorf("scheduler/store: append initial planned row: %w", err)
	}
	return nil
}

func (s *LocalTaskStore) GetTask(ctx context.Context, id string) (TaskRow, error) {
	if id == "" {
		return TaskRow{}, fmt.Errorf("scheduler/store: GetTask requires id")
	}
	rows, err := queries.RunQuery[[]taskRowDB](ctx, s.querier,
		`query ($id: String!) {
			hub { agent { db {
				tasks(filter: {id: {eq: $id}}, limit: 1) {`+taskRowProjection+`}
			}}}
		}`,
		map[string]any{"id": id},
		"hub.agent.db.tasks",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return TaskRow{}, ErrTaskNotFound
		}
		return TaskRow{}, err
	}
	if len(rows) == 0 {
		return TaskRow{}, ErrTaskNotFound
	}
	return rows[0].toRow()
}

func (s *LocalTaskStore) ListTasksBySession(ctx context.Context, sessionID string, opts ListTasksOpts) ([]TaskRow, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("scheduler/store: ListTasksBySession requires sessionID")
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 100
	}
	filter := map[string]any{"owner_session_id": map[string]any{"eq": sessionID}}
	if opts.Status != "" {
		filter["status"] = map[string]any{"eq": opts.Status}
	}
	rows, err := queries.RunQuery[[]taskRowDB](ctx, s.querier,
		`query ($filter: hub_agent_db_tasks_filter, $limit: Int) {
			hub { agent { db {
				tasks(
					filter: $filter,
					order_by: [{field: "created_at", direction: DESC}],
					limit: $limit
				) {`+taskRowProjection+`}
			}}}
		}`,
		map[string]any{"filter": filter, "limit": limit},
		"hub.agent.db.tasks",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return nil, nil
		}
		return nil, err
	}
	return decodeTaskRows(rows)
}

// ListDue returns active tasks whose latest `planned` log row is
// at/after the cutoff. We fetch the candidate set (status=active,
// agent_id) then filter by the planned-row probe in Go — this keeps
// the GraphQL query simple and avoids leaning on engine features
// (LATERAL / correlated subqueries) that aren't uniformly exposed
// through Hugr's filter DSL.
//
// At per-user scale (tens of tasks per agent) the in-Go filter is a
// non-issue. When scale changes, the Postgres path can switch to a
// custom view; the index `idx_task_log_latest` already supports the
// O(log n) per-task probe.
func (s *LocalTaskStore) ListDue(ctx context.Context, agentID string, now time.Time, limit int) ([]TaskRow, error) {
	if agentID == "" {
		return nil, fmt.Errorf("scheduler/store: ListDue requires agentID")
	}
	if limit <= 0 {
		limit = 100
	}
	rows, err := queries.RunQuery[[]taskRowDB](ctx, s.querier,
		`query ($agent: String!, $limit: Int) {
			hub { agent { db {
				tasks(
					filter: {agent_id: {eq: $agent}, status: {eq: "active"}},
					order_by: [{field: "created_at", direction: ASC}],
					limit: $limit
				) {`+taskRowProjection+`}
			}}}
		}`,
		map[string]any{"agent": agentID, "limit": limit},
		"hub.agent.db.tasks",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return nil, nil
		}
		return nil, err
	}

	out := make([]TaskRow, 0, len(rows))
	for _, r := range rows {
		latest, err := s.LatestPlannedFire(ctx, r.ID)
		if err != nil {
			return nil, fmt.Errorf("scheduler/store: ListDue probe %q: %w", r.ID, err)
		}
		if latest == nil {
			// A task without a planned row should not exist (OpenTask
			// inserts fire_seq=1). Skip rather than fire — the
			// invariant violation will surface in tests.
			continue
		}
		if latest.PlannedAt.After(now) {
			continue
		}
		row, err := r.toRow()
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}

func (s *LocalTaskStore) DeleteTask(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("scheduler/store: DeleteTask requires id")
	}
	return queries.RunMutation(ctx, s.querier,
		`mutation ($id: String!) {
			hub { agent { db {
				delete_tasks(filter: {id: {eq: $id}}) { affected_rows }
			}}}
		}`,
		map[string]any{"id": id},
	)
}

func (s *LocalTaskStore) PauseTask(ctx context.Context, id, reason string) error {
	if id == "" {
		return fmt.Errorf("scheduler/store: PauseTask requires id")
	}
	if reason == "" {
		reason = PauseUser
	}
	return s.updateTask(ctx, id, map[string]any{
		"status":       StatusPaused,
		"pause_reason": reason,
	})
}

func (s *LocalTaskStore) ResumeTask(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("scheduler/store: ResumeTask requires id")
	}
	return s.updateTask(ctx, id, map[string]any{
		"status":       StatusActive,
		"pause_reason": nil,
	})
}

func (s *LocalTaskStore) CancelTask(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("scheduler/store: CancelTask requires id")
	}
	return s.updateTask(ctx, id, map[string]any{
		"status": StatusCancelled,
	})
}

func (s *LocalTaskStore) UpdateTaskSpec(ctx context.Context, id string, spec TaskSpec) error {
	if id == "" {
		return fmt.Errorf("scheduler/store: UpdateTaskSpec requires id")
	}
	specBytes, err := json.Marshal(spec)
	if err != nil {
		return fmt.Errorf("scheduler/store: marshal spec: %w", err)
	}
	var specMap map[string]any
	if err := json.Unmarshal(specBytes, &specMap); err != nil {
		return fmt.Errorf("scheduler/store: re-decode spec: %w", err)
	}
	return s.updateTask(ctx, id, map[string]any{"spec": specMap})
}

func (s *LocalTaskStore) updateTask(ctx context.Context, id string, patch map[string]any) error {
	return queries.RunMutation(ctx, s.querier,
		`mutation ($id: String!, $data: hub_agent_db_tasks_mut_data!) {
			hub { agent { db {
				update_tasks(filter: {id: {eq: $id}}, data: $data) { affected_rows }
			}}}
		}`,
		map[string]any{"id": id, "data": patch},
	)
}

// AppendLog is the sole task_log write path. Pure INSERT.
func (s *LocalTaskStore) AppendLog(ctx context.Context, entry TaskLogEntry) error {
	if entry.TaskID == "" {
		return fmt.Errorf("%w: missing TaskID", ErrInvalidLog)
	}
	if entry.AgentID == "" {
		return fmt.Errorf("%w: missing AgentID", ErrInvalidLog)
	}
	if entry.EventType == "" {
		return fmt.Errorf("%w: missing EventType", ErrInvalidLog)
	}
	if !validLogEventType(entry.EventType) {
		return fmt.Errorf("%w: unknown EventType %q — must be one of planned|started|log|completed|failed|skipped|cancelled", ErrInvalidLog, entry.EventType)
	}
	if entry.FireSeq <= 0 {
		return fmt.Errorf("%w: FireSeq must be > 0", ErrInvalidLog)
	}
	if entry.PlannedAt.IsZero() {
		return fmt.Errorf("%w: PlannedAt required", ErrInvalidLog)
	}

	if entry.ID == "" {
		entry.ID = newTaskLogID(entry.TaskID, entry.FireSeq)
	}

	data := map[string]any{
		"id":         entry.ID,
		"task_id":    entry.TaskID,
		"agent_id":   entry.AgentID,
		"fire_seq":   entry.FireSeq,
		"event_type": entry.EventType,
		"planned_at": entry.PlannedAt.UTC().Format(time.RFC3339Nano),
	}
	if entry.SessionID != "" {
		data["session_id"] = entry.SessionID
	}
	if entry.Content != "" {
		data["content"] = entry.Content
	}
	// created_at is server-DEFAULT'd via `NOW()` — we deliberately
	// do NOT accept an override here. Hugr's input mapper treats the
	// default as authoritative for inserts, so a custom value would
	// be silently dropped anyway. Backdating in tests goes through
	// `planned_at` instead — which is the stable per-fire instant
	// the reaper and `.PrevFire` queries key on.
	if entry.Outcome != nil {
		outcomeBytes, err := json.Marshal(entry.Outcome)
		if err != nil {
			return fmt.Errorf("scheduler/store: marshal outcome: %w", err)
		}
		var outcomeMap map[string]any
		if err := json.Unmarshal(outcomeBytes, &outcomeMap); err != nil {
			return fmt.Errorf("scheduler/store: re-decode outcome: %w", err)
		}
		data["outcome"] = outcomeMap
	}

	return queries.RunMutation(ctx, s.querier,
		`mutation ($data: hub_agent_db_task_log_mut_input_data!) {
			hub { agent { db {
				insert_task_log(data: $data) { id }
			}}}
		}`,
		map[string]any{"data": data},
	)
}

func (s *LocalTaskStore) ListLogByTask(ctx context.Context, taskID string, opts ListLogOpts) ([]TaskLogEntry, error) {
	if taskID == "" {
		return nil, fmt.Errorf("scheduler/store: ListLogByTask requires taskID")
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 200
	}
	filter := map[string]any{"task_id": map[string]any{"eq": taskID}}
	if len(opts.EventTypes) > 0 {
		filter["event_type"] = map[string]any{"in": opts.EventTypes}
	}
	if opts.SinceFireSeq > 0 {
		filter["fire_seq"] = map[string]any{"gte": opts.SinceFireSeq}
	}
	rows, err := queries.RunQuery[[]taskLogRowDB](ctx, s.querier,
		`query ($filter: hub_agent_db_task_log_filter, $limit: Int) {
			hub { agent { db {
				task_log(
					filter: $filter,
					order_by: [
						{field: "fire_seq", direction: DESC},
						{field: "created_at", direction: DESC}
					],
					limit: $limit
				) {`+taskLogProjection+`}
			}}}
		}`,
		map[string]any{"filter": filter, "limit": limit},
		"hub.agent.db.task_log",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return nil, nil
		}
		return nil, err
	}
	return decodeLogRows(rows)
}

func (s *LocalTaskStore) LatestPlannedFire(ctx context.Context, taskID string) (*TaskLogEntry, error) {
	return s.latestEntryByEvent(ctx, taskID, LogEventPlanned)
}

func (s *LocalTaskStore) LatestSuccessfulFire(ctx context.Context, taskID string) (*TaskLogEntry, error) {
	return s.latestEntryByEvent(ctx, taskID, LogEventCompleted)
}

func (s *LocalTaskStore) latestEntryByEvent(ctx context.Context, taskID, event string) (*TaskLogEntry, error) {
	if taskID == "" {
		return nil, fmt.Errorf("scheduler/store: latest by event requires taskID")
	}
	rows, err := queries.RunQuery[[]taskLogRowDB](ctx, s.querier,
		`query ($task: String!, $event: String!) {
			hub { agent { db {
				task_log(
					filter: {task_id: {eq: $task}, event_type: {eq: $event}},
					order_by: [
						{field: "fire_seq", direction: DESC},
						{field: "created_at", direction: DESC}
					],
					limit: 1
				) {`+taskLogProjection+`}
			}}}
		}`,
		map[string]any{"task": taskID, "event": event},
		"hub.agent.db.task_log",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return nil, nil
		}
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	entry, err := rows[0].toEntry()
	if err != nil {
		return nil, err
	}
	return &entry, nil
}

// ListInFlightFires returns `started` rows whose fire's `planned_at`
// is older than startedBefore AND have no matching terminal row for
// the same (task_id, fire_seq). Filtering by `planned_at` (not
// `created_at`) keeps the reaper stable across process pauses and
// makes the behaviour testable without relying on server-side wall
// clock for backdating.
//
// Implemented as a two-step probe: fetch the candidate `started`
// rows in a single query, then check each against terminal events.
// At per-fire scale (handful per task) the extra round trips are
// cheap; when scale changes, the Postgres path can switch to a
// custom view keyed on idx_task_log_started.
func (s *LocalTaskStore) ListInFlightFires(ctx context.Context, agentID string, startedBefore time.Time) ([]TaskLogEntry, error) {
	if agentID == "" {
		return nil, fmt.Errorf("scheduler/store: ListInFlightFires requires agentID")
	}
	if startedBefore.IsZero() {
		startedBefore = time.Now()
	}
	rows, err := queries.RunQuery[[]taskLogRowDB](ctx, s.querier,
		`query ($filter: hub_agent_db_task_log_filter) {
			hub { agent { db {
				task_log(
					filter: $filter,
					order_by: [{field: "planned_at", direction: ASC}]
				) {`+taskLogProjection+`}
			}}}
		}`,
		map[string]any{"filter": map[string]any{
			"agent_id":   map[string]any{"eq": agentID},
			"event_type": map[string]any{"eq": LogEventStarted},
			"planned_at": map[string]any{"lt": startedBefore.UTC().Format(time.RFC3339Nano)},
		}},
		"hub.agent.db.task_log",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return nil, nil
		}
		return nil, err
	}

	terminals := []string{LogEventCompleted, LogEventFailed, LogEventCancelled, LogEventSkipped}
	out := make([]TaskLogEntry, 0, len(rows))
	for _, r := range rows {
		terminal, err := queries.RunQuery[[]taskLogRowDB](ctx, s.querier,
			`query ($task: String!, $seq: Int!, $events: [String!]!) {
				hub { agent { db {
					task_log(
						filter: {
							task_id: {eq: $task},
							fire_seq: {eq: $seq},
							event_type: {in: $events}
						},
						limit: 1
					) { id }
				}}}
			}`,
			map[string]any{"task": r.TaskID, "seq": r.FireSeq, "events": terminals},
			"hub.agent.db.task_log",
		)
		if err != nil && !errors.Is(err, types.ErrWrongDataPath) && !errors.Is(err, types.ErrNoData) {
			return nil, err
		}
		if len(terminal) > 0 {
			continue
		}
		entry, err := r.toEntry()
		if err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, nil
}

// decodeTaskRows decodes the JSON-wire form returned by Hugr into
// public [TaskRow] values. Bubbles up the first decode error so
// store consumers fail fast rather than swallowing a half-bad row.
func decodeTaskRows(in []taskRowDB) ([]TaskRow, error) {
	out := make([]TaskRow, 0, len(in))
	for _, r := range in {
		row, err := r.toRow()
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}

func decodeLogRows(in []taskLogRowDB) ([]TaskLogEntry, error) {
	out := make([]TaskLogEntry, 0, len(in))
	for _, r := range in {
		entry, err := r.toEntry()
		if err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, nil
}

// validLogEventType gates the `event_type` column to the seven
// values latest-by-event_type queries expect. A garbage value in
// the column would silently break ListDue / .PrevFire / reaper
// logic (they filter on the enum), so AppendLog rejects unknown
// values upfront rather than letting bad rows land.
func validLogEventType(t string) bool {
	switch t {
	case LogEventPlanned, LogEventStarted, LogEventLog,
		LogEventCompleted, LogEventFailed, LogEventSkipped, LogEventCancelled:
		return true
	}
	return false
}

// newTaskLogID generates the synthetic sortable ID for a log entry
// when AppendLog's caller leaves entry.ID zero. Shape:
// `tlog_<taskID>_<fireSeq>_<rnd>` — same pattern as `evt_…` /
// `note_…`. The crypto/rand suffix avoids collisions on multiple
// rows for the same (task_id, fire_seq) (e.g. planned + started +
// completed of a single fire all share fire_seq).
func newTaskLogID(taskID string, fireSeq int) string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("tlog_%s_%d_%d", taskID, fireSeq, time.Now().UnixNano())
	}
	return fmt.Sprintf("tlog_%s_%d_%s", taskID, fireSeq, hex.EncodeToString(b[:]))
}
