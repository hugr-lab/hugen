package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/scheduler/runner"
	schedstore "github.com/hugr-lab/hugen/pkg/scheduler/store"
)

// withWiredExtension constructs an Extension fully wired to the
// in-memory fake store, a fake host, and a real (but un-started)
// runner.Service so registration calls are honoured without ticks
// firing during the test. Returns a teardown closure the caller
// MUST defer.
func withWiredExtension(t *testing.T) (*Extension, *fakeStore, *fakeHost, *runner.Service) {
	t.Helper()
	store := newFakeStore()
	host := newFakeHost()
	// 10s tick keeps the goroutine alive but never fires during the
	// test (we Stop in teardown long before then).
	r := runner.New(runner.WithTickInterval(10 * time.Second))
	ext := NewExtension(store, nil, "agt_test", nil)
	ext.Bind(host, r)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = r.Stop(ctx)
	})
	return ext, store, host, r
}

// seedTask is a sugar wrapper around fakeStore.OpenTask + the
// post-conditions a real scheduler.callCreate would leave behind
// (status, schedule kind set, etc.). Returns the task ID.
func seedTask(t *testing.T, store *fakeStore, ownerSessionID, taskID string, status string, kind string) {
	t.Helper()
	planned := time.Now().UTC().Add(5 * time.Minute)
	row := schedstore.TaskRow{
		ID:             taskID,
		AgentID:        "agt_test",
		Kind:           kind,
		Status:         schedstore.StatusActive,
		ScheduleKind:   schedstore.ScheduleInterval,
		OwnerSessionID: ownerSessionID,
		Spec: schedstore.TaskSpec{
			Name:         "seeded",
			ScheduleSpec: "5m",
			EndCondition: schedstore.TaskEndCondition{Kind: "until_cancel"},
			Goal:         "Do the seeded thing",
			WakeMessage:  "Hello from the seed",
		},
	}
	if err := store.OpenTask(context.Background(), row, planned); err != nil {
		t.Fatalf("seed OpenTask: %v", err)
	}
	if status != schedstore.StatusActive {
		switch status {
		case schedstore.StatusPaused:
			_ = store.PauseTask(context.Background(), taskID, schedstore.PauseUser)
		case schedstore.StatusCancelled:
			_ = store.CancelTask(context.Background(), taskID)
		}
	}
}

func TestInitState_BootstrapsActiveTasks(t *testing.T) {
	ext, store, _, r := withWiredExtension(t)
	owner := newFakeState("ses-owner-1")

	seedTask(t, store, "ses-owner-1", "tsk_a", schedstore.StatusActive, schedstore.KindSpawn)
	seedTask(t, store, "ses-owner-1", "tsk_b", schedstore.StatusActive, schedstore.KindWake)
	// Owned by another session — must NOT be registered for ses-owner-1.
	seedTask(t, store, "ses-other", "tsk_other", schedstore.StatusActive, schedstore.KindWake)

	if err := ext.InitState(context.Background(), owner); err != nil {
		t.Fatalf("InitState: %v", err)
	}

	// Runner should hold task_tsk_a and task_tsk_b — but not task_tsk_other.
	for _, want := range []string{"task_tsk_a", "task_tsk_b"} {
		st, ok := r.Status(context.Background(), want)
		if !ok {
			t.Errorf("runner missing registration %q", want)
			continue
		}
		if st.Paused {
			t.Errorf("registration %q should be active, got paused", want)
		}
	}
	if _, ok := r.Status(context.Background(), "task_tsk_other"); ok {
		t.Error("foreign-owned task must NOT bootstrap into the runner")
	}
}

func TestInitState_PausedTaskRegistersPaused(t *testing.T) {
	ext, store, _, r := withWiredExtension(t)
	owner := newFakeState("ses-owner-2")

	seedTask(t, store, "ses-owner-2", "tsk_paused", schedstore.StatusPaused, schedstore.KindSpawn)
	if err := ext.InitState(context.Background(), owner); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	st, ok := r.Status(context.Background(), "task_tsk_paused")
	if !ok {
		t.Fatal("paused task should still appear in runner registry")
	}
	if !st.Paused {
		t.Errorf("paused task should register as paused; got active")
	}
}

func TestInitState_CancelledTaskSkipped(t *testing.T) {
	ext, store, _, r := withWiredExtension(t)
	owner := newFakeState("ses-owner-3")

	seedTask(t, store, "ses-owner-3", "tsk_cancel", schedstore.StatusCancelled, schedstore.KindWake)
	if err := ext.InitState(context.Background(), owner); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	if _, ok := r.Status(context.Background(), "task_tsk_cancel"); ok {
		t.Error("cancelled task must NOT bootstrap")
	}
}

func TestInitState_Idempotent(t *testing.T) {
	ext, store, _, r := withWiredExtension(t)
	owner := newFakeState("ses-owner-4")
	seedTask(t, store, "ses-owner-4", "tsk_x", schedstore.StatusActive, schedstore.KindSpawn)

	if err := ext.InitState(context.Background(), owner); err != nil {
		t.Fatalf("first InitState: %v", err)
	}
	if err := ext.InitState(context.Background(), owner); err != nil {
		t.Fatalf("second InitState: %v", err)
	}
	// Single registration — re-entry must NOT double-register or
	// re-fire schedule recomputation. Runner's Register replaces
	// idempotently so the side-effect is observably the same; the
	// guard is on the extension side (bootstrappedSessions).
	if _, ok := r.Status(context.Background(), "task_tsk_x"); !ok {
		t.Fatal("task should still be registered after re-entry")
	}
}

func TestInitState_CronSessionSkipsBootstrap(t *testing.T) {
	ext, store, _, r := withWiredExtension(t)
	owner := newFakeState("ses-cron-x")
	// Stamp the cron envelope so the InitState discriminator skips
	// the bootstrap step (cron sessions are transient fire vessels,
	// not task owners).
	owner.SetValue(protocol.SchedulerFireStateKey, &protocol.FireContext{
		TaskID:    "tsk_owner",
		FireSeq:   1,
	})
	// Seed a task that WOULD be owned by this session — proves the
	// skip is at the extension level, not the store-query level.
	seedTask(t, store, "ses-cron-x", "tsk_cron_owned", schedstore.StatusActive, schedstore.KindSpawn)

	if err := ext.InitState(context.Background(), owner); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	if _, ok := r.Status(context.Background(), "task_tsk_cron_owned"); ok {
		t.Error("cron session must NOT bootstrap any tasks")
	}
}

func TestInitState_NoBindReturnsNoop(t *testing.T) {
	// Construct without Bind — InitState must return nil + skip
	// the store query so misconfigured tests don't panic.
	ext := NewExtension(newFakeStore(), nil, "agt_test", nil)
	owner := newFakeState("ses-unbound")
	if err := ext.InitState(context.Background(), owner); err != nil {
		t.Fatalf("InitState should be a no-op when unbound; got %v", err)
	}
}

// extension import — keeps go vet happy when only test files use it.
var _ extension.Extension = (*Extension)(nil)
