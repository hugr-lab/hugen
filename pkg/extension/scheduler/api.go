package scheduler

import (
	"context"
	"errors"
	"fmt"

	schedstore "github.com/hugr-lab/hugen/pkg/scheduler/store"
)

// Programmatic task-lifecycle surface for non-tool callers (the HTTP task API).
// These mirror the schedule:cancel / delete tool paths — same ownership guard,
// same store + runner coordination — but take an explicit owner session id
// instead of reading it from a tool-call SessionState context.

// ErrTaskNotFound / ErrTaskForbidden let a caller (the adapter) map guard
// failures to 404 / 403 without string-matching.
var (
	ErrTaskNotFound     = errors.New("scheduler: task not found")
	ErrTaskForbidden    = errors.New("scheduler: task not owned by session")
	ErrTaskNotResumable = errors.New("scheduler: task cannot be resumed")
)

// CancelOwnedTask cancels a task on behalf of ownerSessionID (a root session a
// chat binds to). Verifies ownership, sets status=cancelled, and unregisters it
// from the runner so the in-memory fire index — which captured the task row at
// register time — stops firing it. store-only cancel would keep firing.
func (e *Extension) CancelOwnedTask(ctx context.Context, ownerSessionID, taskID string) error {
	row, err := e.ownedTaskByID(ctx, ownerSessionID, taskID)
	if err != nil {
		return err
	}
	if err := e.store.CancelTask(ctx, row.ID); err != nil {
		return fmt.Errorf("scheduler: cancel task %q: %w", row.ID, err)
	}
	if e.runtimeBound() {
		if uerr := e.runner.Unregister(ctx, runnerNameForTask(row.ID)); uerr != nil {
			e.logger.Warn("scheduler: runner unregister (cancel)", "task_id", row.ID, "err", uerr)
		}
	}
	return nil
}

// PauseOwnedTask pauses a task on behalf of ownerSessionID: status=paused +
// runner.Pause so it stops firing but keeps its schedule anchor for resume.
func (e *Extension) PauseOwnedTask(ctx context.Context, ownerSessionID, taskID string) error {
	row, err := e.ownedTaskByID(ctx, ownerSessionID, taskID)
	if err != nil {
		return err
	}
	if err := e.store.PauseTask(ctx, row.ID, schedstore.PauseUser); err != nil {
		return fmt.Errorf("scheduler: pause task %q: %w", row.ID, err)
	}
	if e.runtimeBound() {
		if perr := e.runner.Pause(ctx, runnerNameForTask(row.ID)); perr != nil {
			e.logger.Warn("scheduler: runner pause", "task_id", row.ID, "err", perr)
		}
	}
	return nil
}

// ResumeOwnedTask resumes a paused task on behalf of ownerSessionID: status=
// active + re-registers with the runner (anchored on the latest planned row).
// A cancelled / completed task cannot be resumed → ErrTaskNotResumable.
func (e *Extension) ResumeOwnedTask(ctx context.Context, ownerSessionID, taskID string) error {
	row, err := e.ownedTaskByID(ctx, ownerSessionID, taskID)
	if err != nil {
		return err
	}
	if row.Status == schedstore.StatusCancelled || row.Status == schedstore.StatusCompleted {
		return fmt.Errorf("%w: status %q", ErrTaskNotResumable, row.Status)
	}
	if err := e.store.ResumeTask(ctx, row.ID); err != nil {
		return fmt.Errorf("scheduler: resume task %q: %w", row.ID, err)
	}
	if e.runtimeBound() {
		row.Status = schedstore.StatusActive
		if rerr := e.registerTask(ctx, row); rerr != nil {
			e.logger.Warn("scheduler: re-register on resume", "task_id", row.ID, "err", rerr)
		}
	}
	return nil
}

// DeleteOwnedTask removes a task row on behalf of ownerSessionID. Unregisters
// from the runner first so no in-flight fire references a deleted row.
func (e *Extension) DeleteOwnedTask(ctx context.Context, ownerSessionID, taskID string) error {
	row, err := e.ownedTaskByID(ctx, ownerSessionID, taskID)
	if err != nil {
		return err
	}
	if e.runtimeBound() {
		if uerr := e.runner.Unregister(ctx, runnerNameForTask(row.ID)); uerr != nil {
			e.logger.Warn("scheduler: runner unregister (delete)", "task_id", row.ID, "err", uerr)
		}
	}
	if err := e.store.DeleteTask(ctx, row.ID); err != nil {
		return fmt.Errorf("scheduler: delete task %q: %w", row.ID, err)
	}
	return nil
}

// ownedTaskByID loads a task and asserts ownerSessionID owns it (owner_session_id
// = the root session, matching schedule:create). Maps every miss to
// ErrTaskNotFound except a genuine owner mismatch (ErrTaskForbidden).
func (e *Extension) ownedTaskByID(ctx context.Context, ownerSessionID, taskID string) (schedstore.TaskRow, error) {
	if e.store == nil {
		return schedstore.TaskRow{}, fmt.Errorf("scheduler: TaskStore not wired")
	}
	if ownerSessionID == "" || taskID == "" {
		return schedstore.TaskRow{}, ErrTaskNotFound
	}
	row, err := e.store.GetTask(ctx, taskID)
	if err != nil {
		return schedstore.TaskRow{}, ErrTaskNotFound
	}
	if row.OwnerSessionID != ownerSessionID {
		return schedstore.TaskRow{}, ErrTaskForbidden
	}
	return row, nil
}
