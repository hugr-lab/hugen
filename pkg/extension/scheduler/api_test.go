package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	schedstore "github.com/hugr-lab/hugen/pkg/scheduler/store"
)

// seedTask inserts an active task owned by ownerSessionID into the fake store.
func seedActiveTask(t *testing.T, store *fakeStore, id, ownerSessionID string) {
	t.Helper()
	row := schedstore.TaskRow{
		ID:             id,
		AgentID:        "agt_test",
		Kind:           schedstore.KindWake,
		Status:         schedstore.StatusActive,
		ScheduleKind:   schedstore.ScheduleInterval,
		OwnerSessionID: ownerSessionID,
		Spec:           schedstore.TaskSpec{Name: "n", ScheduleSpec: "5m"},
	}
	if err := store.OpenTask(context.Background(), row, time.Now().UTC()); err != nil {
		t.Fatalf("seed task: %v", err)
	}
}

func TestCancelOwnedTask(t *testing.T) {
	store := newFakeStore()
	ext := NewExtension(store, nil, "agt_test", slog.Default())
	ctx := context.Background()
	seedActiveTask(t, store, "tsk-1", "ses-root")

	// Guard: a different session cannot cancel the task.
	if err := ext.CancelOwnedTask(ctx, "ses-other", "tsk-1"); !errors.Is(err, ErrTaskForbidden) {
		t.Errorf("wrong-owner cancel: got %v, want ErrTaskForbidden", err)
	}
	// Guard: unknown task id.
	if err := ext.CancelOwnedTask(ctx, "ses-root", "nope"); !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("unknown cancel: got %v, want ErrTaskNotFound", err)
	}
	// Happy path: owner cancels → status flips, row kept.
	if err := ext.CancelOwnedTask(ctx, "ses-root", "tsk-1"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	got, err := store.GetTask(ctx, "tsk-1")
	if err != nil {
		t.Fatalf("GetTask after cancel: %v", err)
	}
	if got.Status != schedstore.StatusCancelled {
		t.Errorf("status = %q, want cancelled", got.Status)
	}
}

func TestPauseResumeOwnedTask(t *testing.T) {
	store := newFakeStore()
	ext := NewExtension(store, nil, "agt_test", slog.Default())
	ctx := context.Background()
	seedActiveTask(t, store, "tsk-1", "ses-root")

	// Guard: only the owner can pause.
	if err := ext.PauseOwnedTask(ctx, "ses-other", "tsk-1"); !errors.Is(err, ErrTaskForbidden) {
		t.Errorf("wrong-owner pause: got %v, want ErrTaskForbidden", err)
	}
	// Pause → status flips to paused.
	if err := ext.PauseOwnedTask(ctx, "ses-root", "tsk-1"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if got, _ := store.GetTask(ctx, "tsk-1"); got.Status != schedstore.StatusPaused {
		t.Errorf("after pause status = %q, want paused", got.Status)
	}
	// Resume → back to active.
	if err := ext.ResumeOwnedTask(ctx, "ses-root", "tsk-1"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got, _ := store.GetTask(ctx, "tsk-1"); got.Status != schedstore.StatusActive {
		t.Errorf("after resume status = %q, want active", got.Status)
	}
	// A cancelled task cannot be resumed.
	if err := ext.CancelOwnedTask(ctx, "ses-root", "tsk-1"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if err := ext.ResumeOwnedTask(ctx, "ses-root", "tsk-1"); !errors.Is(err, ErrTaskNotResumable) {
		t.Errorf("resume cancelled: got %v, want ErrTaskNotResumable", err)
	}
}

func TestDeleteOwnedTask(t *testing.T) {
	store := newFakeStore()
	ext := NewExtension(store, nil, "agt_test", slog.Default())
	ctx := context.Background()
	seedActiveTask(t, store, "tsk-1", "ses-root")

	// Guard: not the owner.
	if err := ext.DeleteOwnedTask(ctx, "ses-other", "tsk-1"); !errors.Is(err, ErrTaskForbidden) {
		t.Errorf("wrong-owner delete: got %v, want ErrTaskForbidden", err)
	}
	// Happy path: owner deletes → row gone.
	if err := ext.DeleteOwnedTask(ctx, "ses-root", "tsk-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.GetTask(ctx, "tsk-1"); err == nil {
		t.Errorf("GetTask after delete returned no error; want not-found")
	}
}
