package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/scheduler/runner"
	schedstore "github.com/hugr-lab/hugen/pkg/scheduler/store"
)

// makeWakeTaskRow returns a fully-populated wake-kind task row +
// initial planned log entry stamped on the store, suitable for
// driving a single dispatchWakeFire invocation.
func makeWakeTaskRow(t *testing.T, store *fakeStore, ownerSessionID string) schedstore.TaskRow {
	t.Helper()
	row := schedstore.TaskRow{
		ID:             "tsk_wake_1",
		AgentID:        "agt_test",
		Kind:           schedstore.KindWake,
		Status:         schedstore.StatusActive,
		ScheduleKind:   schedstore.ScheduleInterval,
		OwnerSessionID: ownerSessionID,
		Spec: schedstore.TaskSpec{
			Name:         "wake",
			ScheduleSpec: "1h",
			EndCondition: schedstore.TaskEndCondition{Kind: "until_cancel"},
			WakeMessage:  "Time to check {{ .Inputs.region }} dashboard",
			Inputs:       map[string]any{"region": "EU"},
		},
	}
	if err := store.OpenTask(context.Background(), row, time.Now().UTC()); err != nil {
		t.Fatalf("seed OpenTask: %v", err)
	}
	return row
}

// minimalDeps wires a fireDeps with safe defaults — no spawn rendezvous
// closures (cron-spawn dispatch isn't exercised by these tests), but
// pauseFn is tracked via a recorder so render-fail / dead-owner tests
// can assert that the runner-side pause path runs.
func minimalDeps(t *testing.T, store *fakeStore, host *fakeHost, pauses *[]string) fireDeps {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(testWriter{t: t}, nil))
	return fireDeps{
		store:   store,
		host:    host,
		agentID: "agt_test",
		logger:  logger,
		pauseFn: func(taskID string) error {
			*pauses = append(*pauses, taskID)
			return nil
		},
	}
}

func TestDispatchWakeFire_DeliversRenderedMessage(t *testing.T) {
	store := newFakeStore()
	host := newFakeHost()
	host.markAlive("ses-owner-wake")
	var pauses []string
	row := makeWakeTaskRow(t, store, "ses-owner-wake")
	deps := minimalDeps(t, store, host, &pauses)
	fn := buildFireFn(row, deps)

	out, err := fn(context.Background(), runner.FireMeta{
		Name:      "task_tsk_wake_1",
		FireSeq:   1,
		PlannedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("fire fn: %v", err)
	}
	if out.Summary == "" {
		t.Errorf("outcome summary must not be empty")
	}

	deliveries := host.deliveries()
	if len(deliveries) != 1 {
		t.Fatalf("expected 1 delivered frame, got %d", len(deliveries))
	}
	user, ok := deliveries[0].Frame.(*protocol.UserMessage)
	if !ok {
		t.Fatalf("delivered frame type=%T", deliveries[0].Frame)
	}
	if user.Payload.Text != "Time to check EU dashboard" {
		t.Errorf("rendered wake message = %q", user.Payload.Text)
	}

	// Task log: planned (from OpenTask), started, completed, then
	// a new planned for fire #2 (interval schedule).
	events := store.snapshotLog()
	if len(events) < 3 {
		t.Fatalf("expected >= 3 task_log rows, got %d (%v)", len(events), events)
	}
	wantTypes := map[string]bool{
		schedstore.LogEventStarted:   false,
		schedstore.LogEventCompleted: false,
		schedstore.LogEventPlanned:   false,
	}
	for _, e := range events {
		if _, ok := wantTypes[e.EventType]; ok {
			wantTypes[e.EventType] = true
		}
	}
	for et, seen := range wantTypes {
		if !seen {
			t.Errorf("missing %s event in task_log", et)
		}
	}

	if len(pauses) != 0 {
		t.Errorf("happy wake fire must not invoke pauseFn; got %v", pauses)
	}
}

// TestDispatchWakeFire_DeliverFailureStillSchedulesNext asserts that a
// fire whose delivery fails still advances the schedule: it logs the
// failure AND writes the next planned row + reschedules, so one bad fire
// (a transient inbox error) does not stall a recurring task. Mirrors the
// spawn-fire spawn_failed/fire_timeout path.
func TestDispatchWakeFire_DeliverFailureStillSchedulesNext(t *testing.T) {
	store := newFakeStore()
	host := newFakeHost()
	host.markAlive("ses-owner-wake")
	host.deliverErr = errors.New("inbox closed")
	var pauses []string
	row := makeWakeTaskRow(t, store, "ses-owner-wake")
	deps := minimalDeps(t, store, host, &pauses)
	var rescheduled []time.Time
	deps.rescheduleFn = func(_ string, at time.Time) error {
		rescheduled = append(rescheduled, at)
		return nil
	}
	fn := buildFireFn(row, deps)

	planned := time.Now().UTC()
	if _, err := fn(context.Background(), runner.FireMeta{
		Name:      "task_tsk_wake_1",
		FireSeq:   1,
		PlannedAt: planned,
	}); err == nil {
		t.Fatal("deliver failure must surface as a fire error")
	}

	var sawFailed, sawNextPlanned bool
	for _, e := range store.snapshotLog() {
		if e.EventType == schedstore.LogEventFailed {
			sawFailed = true
		}
		if e.EventType == schedstore.LogEventPlanned && e.FireSeq == 2 {
			sawNextPlanned = true
		}
	}
	if !sawFailed {
		t.Error("expected a failed task_log row")
	}
	if !sawNextPlanned {
		t.Error("failed fire must still write the next planned row (fire #2)")
	}
	if len(rescheduled) != 1 {
		t.Fatalf("failed fire must reschedule the next fire; got %d reschedules", len(rescheduled))
	}
	if wantNext := planned.Add(time.Hour); !rescheduled[0].Equal(wantNext) {
		t.Errorf("reschedule at=%v, want one interval later %v", rescheduled[0], wantNext)
	}
	// A delivery failure is transient, not pause-worthy.
	if len(pauses) != 0 {
		t.Errorf("deliver-failure must not pause the task; got %v", pauses)
	}
}

// TestDispatchWakeFire_FailedFinalFireRespectsCount asserts the
// failure-reschedule still honours the count end-condition: a failed
// fire is an attempt, and a failed FINAL fire (seq == count) terminates
// the task instead of rescheduling — one bad run neither stalls the
// schedule (see above) nor runs past its count.
func TestDispatchWakeFire_FailedFinalFireRespectsCount(t *testing.T) {
	store := newFakeStore()
	host := newFakeHost()
	host.markAlive("ses-owner-count")
	host.deliverErr = errors.New("inbox closed")
	row := schedstore.TaskRow{
		ID:             "tsk_wake_count1",
		AgentID:        "agt_test",
		Kind:           schedstore.KindWake,
		Status:         schedstore.StatusActive,
		ScheduleKind:   schedstore.ScheduleInterval,
		OwnerSessionID: "ses-owner-count",
		Spec: schedstore.TaskSpec{
			Name:         "once",
			ScheduleSpec: "1h",
			EndCondition: schedstore.TaskEndCondition{Kind: "count", Spec: "1"},
			WakeMessage:  "ping",
		},
	}
	if err := store.OpenTask(context.Background(), row, time.Now().UTC()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var pauses []string
	deps := minimalDeps(t, store, host, &pauses)
	var rescheduled []time.Time
	deps.rescheduleFn = func(_ string, at time.Time) error {
		rescheduled = append(rescheduled, at)
		return nil
	}
	fn := buildFireFn(row, deps)

	if _, err := fn(context.Background(), runner.FireMeta{
		Name:      "task_tsk_wake_count1",
		FireSeq:   1, // the only allowed fire (count=1)
		PlannedAt: time.Now().UTC(),
	}); err == nil {
		t.Fatal("deliver failure must surface as a fire error")
	}

	// count=1 reached on this (failed) fire → no next planned, no
	// reschedule, task cancelled.
	for _, e := range store.snapshotLog() {
		if e.EventType == schedstore.LogEventPlanned && e.FireSeq == 2 {
			t.Error("count=1 reached: must not write a next planned row")
		}
	}
	if len(rescheduled) != 0 {
		t.Errorf("count=1 reached: must not reschedule; got %v", rescheduled)
	}
	updated, err := store.GetTask(context.Background(), row.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if updated.Status != schedstore.StatusCancelled {
		t.Errorf("status = %q, want cancelled (count reached)", updated.Status)
	}
}

func TestDispatchWakeFire_DeadOwnerPauses(t *testing.T) {
	store := newFakeStore()
	host := newFakeHost()
	// owner NOT marked alive — simulates a terminated owner session.
	var pauses []string

	row := makeWakeTaskRow(t, store, "ses-ghost")
	deps := minimalDeps(t, store, host, &pauses)
	fn := buildFireFn(row, deps)

	out, err := fn(context.Background(), runner.FireMeta{
		Name:      "task_tsk_wake_1",
		FireSeq:   1,
		PlannedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("fire fn returned error: %v", err)
	}
	if out.Reason != schedstore.PauseOwnerTerminated {
		t.Errorf("outcome.reason = %q, want %s", out.Reason, schedstore.PauseOwnerTerminated)
	}

	// Task auto-paused in the store.
	updated, err := store.GetTask(context.Background(), row.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if updated.Status != schedstore.StatusPaused {
		t.Errorf("status = %q, want paused", updated.Status)
	}
	if updated.PauseReason != schedstore.PauseOwnerTerminated {
		t.Errorf("pause_reason = %q", updated.PauseReason)
	}

	// Runner-side pause invoked (the load-bearing fix vs the
	// pre-refactor "store paused but runner keeps firing" bug).
	if len(pauses) != 1 || pauses[0] != row.ID {
		t.Errorf("pauseFn invocations = %v; want [%q]", pauses, row.ID)
	}

	// No UserMessage delivered.
	if d := host.deliveries(); len(d) != 0 {
		t.Errorf("dead owner should not receive a UserMessage; got %d", len(d))
	}
}

func TestDispatchWakeFire_RenderFailurePauses(t *testing.T) {
	store := newFakeStore()
	host := newFakeHost()
	host.markAlive("ses-owner-render")
	var pauses []string

	row := makeWakeTaskRow(t, store, "ses-owner-render")
	// Inject an unparsable template — `{{ ` with no closing braces.
	row.Spec.WakeMessage = "{{ .Inputs.region "
	if err := store.UpdateTaskSpec(context.Background(), row.ID, row.Spec); err != nil {
		t.Fatalf("UpdateTaskSpec: %v", err)
	}

	deps := minimalDeps(t, store, host, &pauses)
	fn := buildFireFn(row, deps)

	_, err := fn(context.Background(), runner.FireMeta{
		Name:      "task_tsk_wake_1",
		FireSeq:   1,
		PlannedAt: time.Now().UTC(),
	})
	if err == nil {
		t.Fatal("expected error on render failure")
	}

	updated, _ := store.GetTask(context.Background(), row.ID)
	if updated.Status != schedstore.StatusPaused {
		t.Errorf("render failure should pause task; got status %q", updated.Status)
	}
	if updated.PauseReason != schedstore.PauseRenderFailed {
		t.Errorf("pause_reason = %q, want render_failed", updated.PauseReason)
	}
	if d := host.deliveries(); len(d) != 0 {
		t.Errorf("render failure should not deliver any frame; got %d", len(d))
	}
	if len(pauses) != 1 || pauses[0] != row.ID {
		t.Errorf("pauseFn must run on render failure; got %v", pauses)
	}
}

func TestDispatchSpawnFire_DeadOwnerPauses(t *testing.T) {
	// Spawn-fire dispatch needs a live owner — the fire fn calls
	// host.Get(ownerID) first. If the owner isn't alive we pause
	// without attempting Spawn. This replaces the pre-refactor
	// host.Open error path (Open no longer exists on SessionHost).
	store := newFakeStore()
	host := newFakeHost() // owner NOT marked alive
	var pauses []string

	row := schedstore.TaskRow{
		ID:             "tsk_spawn_dead",
		AgentID:        "agt_test",
		Kind:           schedstore.KindSpawn,
		Status:         schedstore.StatusActive,
		ScheduleKind:   schedstore.ScheduleInterval,
		OwnerSessionID: "ses-ghost-spawn",
		Spec: schedstore.TaskSpec{
			Name:         "spawn-dead",
			ScheduleSpec: "1h",
			EndCondition: schedstore.TaskEndCondition{Kind: "until_cancel"},
			Goal:         "doesn't matter — owner is dead",
		},
	}
	if err := store.OpenTask(context.Background(), row, time.Now().UTC()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	deps := minimalDeps(t, store, host, &pauses)
	deps.takeSpawnToken = func() int64 { return 0 }
	deps.stashFire = func(string, *protocol.FireContext) {}
	deps.releaseFire = func(string) {}
	fn := buildFireFn(row, deps)

	out, err := fn(context.Background(), runner.FireMeta{
		Name:      "task_tsk_spawn_dead",
		FireSeq:   1,
		PlannedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("fire fn returned error: %v", err)
	}
	if out.Reason != schedstore.PauseOwnerTerminated {
		t.Errorf("outcome.reason = %q, want %s", out.Reason, schedstore.PauseOwnerTerminated)
	}

	updated, _ := store.GetTask(context.Background(), row.ID)
	if updated.Status != schedstore.StatusPaused {
		t.Errorf("status = %q, want paused", updated.Status)
	}
	if len(pauses) != 1 || pauses[0] != row.ID {
		t.Errorf("pauseFn must run on dead-owner spawn; got %v", pauses)
	}
}

// Note: the spawn-fire INPUT-render-failure auto-pause (symmetric to
// TestDispatchWakeFire_RenderFailurePauses) is not unit-testable with
// the fake host — dispatchSpawnFire requires a non-nil owner *session.
// Session past the owner check before it reaches the input render, and
// fakeHost.Get returns (nil, true). The behaviour mirrors the tested
// wake path; template.RenderInputs error handling is covered in
// pkg/runtime/template.

// TestMaybeScheduleNext_CronRecomputes asserts the recompute path
// for a recurring cron task: after a fire it inserts the next
// `planned` row at the cron-derived instant and reschedules the runner
// registration's next fire to it. "0 9 * * 1" advances from one Monday
// 09:00 to the next.
func TestMaybeScheduleNext_CronRecomputes(t *testing.T) {
	store := newFakeStore()
	row := schedstore.TaskRow{
		ID:           "tsk_cron_1",
		AgentID:      "agt_test",
		Kind:         schedstore.KindWake,
		Status:       schedstore.StatusActive,
		ScheduleKind: schedstore.ScheduleCron,
		Spec: schedstore.TaskSpec{
			Name:         "weekly",
			ScheduleSpec: "0 9 * * 1",
			EndCondition: schedstore.TaskEndCondition{Kind: "until_cancel"},
		},
	}
	planned := time.Date(2024, 1, 1, 9, 0, 0, 0, time.UTC) // Monday
	if err := store.OpenTask(context.Background(), row, planned); err != nil {
		t.Fatalf("seed OpenTask: %v", err)
	}

	var rescheduled []time.Time
	deps := fireDeps{
		store:   store,
		agentID: "agt_test",
		logger:  slog.New(slog.NewTextHandler(testWriter{t: t}, nil)),
		rescheduleFn: func(_ string, at time.Time) error {
			rescheduled = append(rescheduled, at)
			return nil
		},
	}

	maybeScheduleNext(context.Background(), row, runner.FireMeta{FireSeq: 1, PlannedAt: planned}, deps)

	wantNext := time.Date(2024, 1, 8, 9, 0, 0, 0, time.UTC) // following Monday
	pl, err := store.LatestPlannedFire(context.Background(), row.ID)
	if err != nil || pl == nil {
		t.Fatalf("LatestPlannedFire: %v (%v)", err, pl)
	}
	if !pl.PlannedAt.Equal(wantNext) {
		t.Errorf("next planned_at=%v, want %v", pl.PlannedAt, wantNext)
	}
	if pl.FireSeq != 2 {
		t.Errorf("next fire_seq=%d, want 2", pl.FireSeq)
	}
	if len(rescheduled) != 1 || !rescheduled[0].Equal(wantNext) {
		t.Errorf("reschedule = %v, want one at %v", rescheduled, wantNext)
	}
}

// TestMaybeScheduleNext_IntervalOverdueFiresVerbatim asserts the
// long-fire fix: when a fire ran longer than the interval, the next
// instant (prevPlanned + interval) is already in the past. The refactor
// writes it VERBATIM (no clamp) and reschedules the runner registration
// to it; the runner then fires it on the next tick because nextFireAt <=
// now (overdue catch-up), so a task slower than its interval keeps its
// cadence back-to-back. (Pre-refactor runner.Once(past) returned zero
// and the fire was silently dropped — see the runner-level
// TestRegister_WithInitialFireAtPastFires / TestReschedule for the
// firing half.)
func TestMaybeScheduleNext_IntervalOverdueFiresVerbatim(t *testing.T) {
	store := newFakeStore()
	row := schedstore.TaskRow{
		ID:           "tsk_overdue",
		AgentID:      "agt_test",
		Kind:         schedstore.KindSpawn,
		Status:       schedstore.StatusActive,
		ScheduleKind: schedstore.ScheduleInterval,
		Spec: schedstore.TaskSpec{
			Name:         "slow",
			ScheduleSpec: "2m",
			EndCondition: schedstore.TaskEndCondition{Kind: "until_cancel"},
		},
	}
	// Fire #1 was planned at 12:00 and ran 9m (≫ the 2m interval), so the
	// logical next (12:02) is already ~7m in the past at completion.
	planned := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	if err := store.OpenTask(context.Background(), row, planned); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var rescheduled []time.Time
	deps := fireDeps{
		store:   store,
		agentID: "agt_test",
		logger:  slog.New(slog.NewTextHandler(testWriter{t: t}, nil)),
		rescheduleFn: func(_ string, at time.Time) error {
			rescheduled = append(rescheduled, at)
			return nil
		},
	}

	maybeScheduleNext(context.Background(), row, runner.FireMeta{FireSeq: 1, PlannedAt: planned}, deps)

	wantNext := planned.Add(2 * time.Minute) // logical next, written verbatim — NOT clamped to now
	pl, err := store.LatestPlannedFire(context.Background(), row.ID)
	if err != nil || pl == nil {
		t.Fatalf("LatestPlannedFire: %v (%v)", err, pl)
	}
	if !pl.PlannedAt.Equal(wantNext) {
		t.Errorf("overdue next planned_at=%v, want verbatim logical %v (no clamp)", pl.PlannedAt, wantNext)
	}
	if len(rescheduled) != 1 || !rescheduled[0].Equal(wantNext) {
		t.Fatalf("overdue re-fire must reschedule to the verbatim instant; got %v", rescheduled)
	}
}

// TestMaybeScheduleNext_CronEndConditionCount stops a cron task once
// the count end-condition is reached — no further planned row, the
// task is cancelled.
func TestMaybeScheduleNext_CronEndConditionCount(t *testing.T) {
	store := newFakeStore()
	row := schedstore.TaskRow{
		ID:           "tsk_cron_count",
		AgentID:      "agt_test",
		Kind:         schedstore.KindWake,
		Status:       schedstore.StatusActive,
		ScheduleKind: schedstore.ScheduleCron,
		Spec: schedstore.TaskSpec{
			Name:         "twice",
			ScheduleSpec: "0 9 * * 1",
			EndCondition: schedstore.TaskEndCondition{Kind: "count", Spec: "2"},
		},
	}
	planned := time.Date(2024, 1, 1, 9, 0, 0, 0, time.UTC)
	if err := store.OpenTask(context.Background(), row, planned); err != nil {
		t.Fatalf("seed OpenTask: %v", err)
	}
	deps := fireDeps{
		store:        store,
		agentID:      "agt_test",
		logger:       slog.New(slog.NewTextHandler(testWriter{t: t}, nil)),
		rescheduleFn: func(string, time.Time) error { return nil },
	}
	// fire_seq 2 completed; count=2 reached → cancel, no new planned.
	maybeScheduleNext(context.Background(), row, runner.FireMeta{FireSeq: 2, PlannedAt: planned}, deps)

	got, err := store.GetTask(context.Background(), row.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != schedstore.StatusCancelled {
		t.Errorf("status=%q, want cancelled after end-condition", got.Status)
	}
}

// TestMaybeScheduleNext_CronBadExprPauses asserts a hand-edited /
// drifted row carrying an unparseable cron spec pauses rather than
// crashing the fire loop (create validates, but the store is also
// writable via GraphQL).
func TestMaybeScheduleNext_CronBadExprPauses(t *testing.T) {
	store := newFakeStore()
	row := schedstore.TaskRow{
		ID:           "tsk_cron_bad",
		AgentID:      "agt_test",
		Kind:         schedstore.KindWake,
		Status:       schedstore.StatusActive,
		ScheduleKind: schedstore.ScheduleCron,
		Spec: schedstore.TaskSpec{
			Name:         "broken",
			ScheduleSpec: "not a cron",
			EndCondition: schedstore.TaskEndCondition{Kind: "until_cancel"},
		},
	}
	planned := time.Date(2024, 1, 1, 9, 0, 0, 0, time.UTC)
	if err := store.OpenTask(context.Background(), row, planned); err != nil {
		t.Fatalf("seed OpenTask: %v", err)
	}
	var pauses []string
	deps := fireDeps{
		store:   store,
		agentID: "agt_test",
		logger:  slog.New(slog.NewTextHandler(testWriter{t: t}, nil)),
		pauseFn: func(taskID string) error { pauses = append(pauses, taskID); return nil },
	}
	maybeScheduleNext(context.Background(), row, runner.FireMeta{FireSeq: 1, PlannedAt: planned}, deps)

	got, err := store.GetTask(context.Background(), row.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != schedstore.StatusPaused {
		t.Errorf("status=%q, want paused on bad cron expr", got.Status)
	}
	if len(pauses) != 1 {
		t.Errorf("pauseFn calls=%v, want one", pauses)
	}
}

func TestHashJSON_StableAcrossKeyOrder(t *testing.T) {
	a := map[string]any{"foo": 1, "bar": "x"}
	b := map[string]any{"bar": "x", "foo": 1}
	if hashJSON(a) != hashJSON(b) {
		t.Errorf("hashJSON not stable across key order")
	}
}

func TestHashJSON_DifferentValuesDiffer(t *testing.T) {
	a := hashJSON(map[string]any{"foo": 1})
	b := hashJSON(map[string]any{"foo": 2})
	if a == b {
		t.Errorf("hashJSON collision on distinct values")
	}
}

// testWriter routes structured logs to the test logger so failed
// runs surface scheduler-side warnings without polluting stdout on
// the happy path.
type testWriter struct {
	t *testing.T
}

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}
