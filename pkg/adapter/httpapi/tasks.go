package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	schedext "github.com/hugr-lab/hugen/pkg/extension/scheduler"
	schedstore "github.com/hugr-lab/hugen/pkg/scheduler/store"
)

// taskReader is the read subset of scheduler/store.TaskStore the tasks endpoint
// needs. schedstore.TaskStore satisfies it; wired via WithTaskStore. Kept narrow
// so the adapter can't reach the lifecycle-mutating half of the store.
type taskReader interface {
	ListTasksBySession(ctx context.Context, sessionID string, opts schedstore.ListTasksOpts) ([]schedstore.TaskRow, error)
	LatestPlannedFire(ctx context.Context, taskID string) (*schedstore.TaskLogEntry, error)
	CountTasksBySession(ctx context.Context, sessionID string) (map[string]int, error)
}

// taskController is the write surface for the task lifecycle — cancel / delete.
// Backed by the scheduler Extension so each op coordinates the store AND the
// in-memory runner index (a store-only cancel would keep firing). Ownership is
// re-checked inside against the owner session id the adapter passes.
type taskController interface {
	CancelOwnedTask(ctx context.Context, ownerSessionID, taskID string) error
	DeleteOwnedTask(ctx context.Context, ownerSessionID, taskID string) error
	PauseOwnedTask(ctx context.Context, ownerSessionID, taskID string) error
	ResumeOwnedTask(ctx context.Context, ownerSessionID, taskID string) error
}

// taskEndConditionDTO mirrors schedstore.TaskEndCondition: when a recurring task
// stops firing. Kind "until_cancel" | "count" | "until"; Spec holds the count or
// an RFC3339 instant (empty for until_cancel).
type taskEndConditionDTO struct {
	Kind string `json:"kind"`
	Spec string `json:"spec,omitempty"`
}

// sessionTaskDTO is the wire shape for one scheduled task owned by a session.
// It projects the operator-visible columns the TUI `/tasks` command renders,
// plus the next planned fire resolved from task_log — so a UI can show "next
// fire" without a second query.
type sessionTaskDTO struct {
	ID           string              `json:"id"`
	Name         string              `json:"name"`
	Description  string              `json:"description,omitempty"`
	Kind         string              `json:"kind"`          // wake | spawn
	ScheduleKind string              `json:"schedule_kind"` // once_in | once_at | cron | interval
	ScheduleSpec string              `json:"schedule_spec,omitempty"`
	Timezone     string              `json:"timezone,omitempty"`
	Status       string              `json:"status"` // active | paused | cancelled | completed
	PauseReason  string              `json:"pause_reason,omitempty"`
	EndCondition taskEndConditionDTO `json:"end_condition"`
	NextFire     *time.Time          `json:"next_fire,omitempty"`
	CreatedAt    time.Time           `json:"created_at"`
}

// sessionTasksResponse wraps the list with an archived count so a UI can offer a
// "show all" affordance (and a badge) even when the live view is empty —
// archived = cancelled + completed, the statuses hidden from the live filter.
type sessionTasksResponse struct {
	Tasks         []sessionTaskDTO `json:"tasks"`
	ArchivedCount int              `json:"archived_count"`
}

// handleListSessionTasks returns the scheduled tasks owned by a session
// (owner_session_id = the root session a chat binds to), newest planned fire
// resolved per row. Filtered server-side so a session with a long cancelled /
// completed history stays cheap:
//
//   - no ?status         → live tasks only (active + paused) — the default view
//   - ?status=all        → every status (the archive view)
//   - ?status=a,b        → exactly those statuses (comma-separated)
//
// archived_count (cancelled + completed) always accompanies the list via one
// bucket aggregation. Ownership-checked. Empty response (200) when the scheduler
// is not wired, so a UI can call it unconditionally.
func (a *Adapter) handleListSessionTasks(w http.ResponseWriter, r *http.Request) {
	_, id, ok := a.ownedRequest(w, r)
	if !ok {
		return
	}
	if a.taskStore == nil {
		writeJSON(w, http.StatusOK, sessionTasksResponse{Tasks: []sessionTaskDTO{}})
		return
	}
	var opts schedstore.ListTasksOpts
	switch s := strings.TrimSpace(r.URL.Query().Get("status")); s {
	case "":
		opts.Statuses = []string{schedstore.StatusActive, schedstore.StatusPaused} // live default
	case "all":
		// no status filter — every task, including cancelled / completed history
	default:
		opts.Statuses = strings.Split(s, ",")
	}
	rows, err := a.taskStore.ListTasksBySession(r.Context(), id, opts)
	if err != nil {
		a.logger.Error("httpapi: list session tasks", "id", id, "err", err)
		httpError(w, http.StatusInternalServerError, "list tasks failed")
		return
	}
	out := make([]sessionTaskDTO, 0, len(rows))
	for _, row := range rows {
		d := sessionTaskDTO{
			ID:           row.ID,
			Name:         row.Spec.Name,
			Description:  row.Spec.Description,
			Kind:         row.Kind,
			ScheduleKind: row.ScheduleKind,
			ScheduleSpec: row.Spec.ScheduleSpec,
			Timezone:     row.Spec.Timezone,
			Status:       row.Status,
			PauseReason:  row.PauseReason,
			EndCondition: taskEndConditionDTO{Kind: row.Spec.EndCondition.Kind, Spec: row.Spec.EndCondition.Spec},
			CreatedAt:    row.CreatedAt,
		}
		// Resolve the next planned fire from task_log (same N+1 the TUI /tasks
		// and schedule:list surfaces use). Best-effort — a missing anchor just
		// leaves next_fire null.
		if planned, perr := a.taskStore.LatestPlannedFire(r.Context(), row.ID); perr == nil && planned != nil {
			t := planned.PlannedAt
			d.NextFire = &t
		}
		out = append(out, d)
	}
	// archived = cancelled + completed, so the UI can badge "N archived" even in
	// the live view. Best-effort — a count failure just omits the badge.
	archived := 0
	if counts, cerr := a.taskStore.CountTasksBySession(r.Context(), id); cerr == nil {
		archived = counts[schedstore.StatusCancelled] + counts[schedstore.StatusCompleted]
	}
	writeJSON(w, http.StatusOK, sessionTasksResponse{Tasks: out, ArchivedCount: archived})
}

// handleCancelSessionTask cancels a scheduled task owned by the session (stops
// it firing but keeps the row/history). POST …/tasks/{taskId}/cancel.
func (a *Adapter) handleCancelSessionTask(w http.ResponseWriter, r *http.Request) {
	a.mutateSessionTask(w, r, func(ctx context.Context, sid, tid string) error {
		return a.taskCtl.CancelOwnedTask(ctx, sid, tid)
	}, "cancelled")
}

// handleDeleteSessionTask removes a scheduled task owned by the session.
// DELETE …/tasks/{taskId}.
func (a *Adapter) handleDeleteSessionTask(w http.ResponseWriter, r *http.Request) {
	a.mutateSessionTask(w, r, func(ctx context.Context, sid, tid string) error {
		return a.taskCtl.DeleteOwnedTask(ctx, sid, tid)
	}, "deleted")
}

// handlePauseSessionTask pauses a scheduled task (stops firing, keeps its
// schedule for resume). POST …/tasks/{taskId}/pause.
func (a *Adapter) handlePauseSessionTask(w http.ResponseWriter, r *http.Request) {
	a.mutateSessionTask(w, r, func(ctx context.Context, sid, tid string) error {
		return a.taskCtl.PauseOwnedTask(ctx, sid, tid)
	}, "paused")
}

// handleResumeSessionTask resumes a paused task. POST …/tasks/{taskId}/resume.
func (a *Adapter) handleResumeSessionTask(w http.ResponseWriter, r *http.Request) {
	a.mutateSessionTask(w, r, func(ctx context.Context, sid, tid string) error {
		return a.taskCtl.ResumeOwnedTask(ctx, sid, tid)
	}, "resumed")
}

// mutateSessionTask is the shared cancel/delete plumbing: session-ownership
// check, controller wiring guard, task id, error mapping (404 not-found /
// 403 not-owned / 500 else), and the ack body.
func (a *Adapter) mutateSessionTask(w http.ResponseWriter, r *http.Request, op func(context.Context, string, string) error, verb string) {
	_, id, ok := a.ownedRequest(w, r)
	if !ok {
		return
	}
	if a.taskCtl == nil {
		httpError(w, http.StatusNotImplemented, "scheduler not enabled")
		return
	}
	taskID := r.PathValue("taskId")
	if taskID == "" {
		httpError(w, http.StatusBadRequest, "task id required")
		return
	}
	switch err := op(r.Context(), id, taskID); {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]string{"id": taskID, "status": verb})
	case errors.Is(err, schedext.ErrTaskNotFound):
		httpError(w, http.StatusNotFound, "task not found")
	case errors.Is(err, schedext.ErrTaskForbidden):
		httpError(w, http.StatusForbidden, "task not owned by this session")
	case errors.Is(err, schedext.ErrTaskNotResumable):
		httpError(w, http.StatusConflict, "task cannot be resumed")
	default:
		a.logger.Error("httpapi: mutate session task", "session", id, "task", taskID, "op", verb, "err", err)
		httpError(w, http.StatusInternalServerError, verb+" task failed")
	}
}
