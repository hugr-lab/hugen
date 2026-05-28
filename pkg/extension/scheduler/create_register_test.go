package scheduler

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/scheduler/runner"
	schedstore "github.com/hugr-lab/hugen/pkg/scheduler/store"
)

// TestCreate_RegistersWithRunner asserts the load-bearing 6.1c
// invariant: `schedule:create` not only persists the row + initial
// planned log entry, but also registers a runner fn so the task
// fires without an InitState bootstrap. (6.1b stopped at the
// persistence layer; 6.1c closes the loop.)
func TestCreate_RegistersWithRunner(t *testing.T) {
	ext, store, _, r := withWiredExtension(t)
	owner := newFakeState("ses-owner-create")

	planned := time.Now().UTC().Add(time.Hour)
	body := callTool(t, ext, owner, "create", map[string]any{
		"kind":               schedstore.KindWake,
		"schedule_kind":      schedstore.ScheduleInterval,
		"schedule_spec":      "5m",
		"initial_planned_at": planned.Format(time.RFC3339Nano),
		"name":               "test task",
		"wake_message":       "ping",
		"end_condition":      map[string]any{"kind": "until_cancel"},
	})

	var out createOutput
	if err := jsonUnmarshalForTest(body, &out); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, body)
	}
	if out.Status != schedstore.StatusActive {
		t.Errorf("status=%q, want active", out.Status)
	}
	if out.TaskID == "" {
		t.Fatal("task id missing from create response")
	}

	// Row persisted.
	row, err := store.GetTask(context.Background(), out.TaskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if row.OwnerSessionID != "ses-owner-create" {
		t.Errorf("owner_session_id=%q", row.OwnerSessionID)
	}

	// Runner-side registration installed.
	st, ok := r.Status(context.Background(), "task_"+out.TaskID)
	if !ok {
		t.Fatalf("runner missing registration for new task")
	}
	if st.Paused {
		t.Errorf("freshly-created task should not be paused")
	}
	if st.NextFireAt.IsZero() {
		t.Errorf("next_fire_at not anchored after create")
	}
}

// TestCreate_NoBindLeavesStore exercises the early-boot path: the
// extension can persist a task before Bind installed the runtime
// host + runner. The row still lands; the runner registration
// happens at the next InitState (resume / restart).
func TestCreate_NoBindLeavesStore(t *testing.T) {
	store := newFakeStore()
	ext := NewExtension(store, nil, "agt_test", nil)
	owner := newFakeState("ses-no-bind")

	body := callTool(t, ext, owner, "create", map[string]any{
		"kind":               schedstore.KindWake,
		"schedule_kind":      schedstore.ScheduleInterval,
		"schedule_spec":      "5m",
		"initial_planned_at": time.Now().UTC().Format(time.RFC3339Nano),
		"name":               "no bind",
		"wake_message":       "ping",
		"end_condition":      map[string]any{"kind": "until_cancel"},
	})
	var out createOutput
	if err := jsonUnmarshalForTest(body, &out); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, body)
	}
	if _, err := store.GetTask(context.Background(), out.TaskID); err != nil {
		t.Fatalf("row should persist even without Bind: %v", err)
	}
}

// TestCreate_CronAcceptedAndPlannedDerived asserts schedule_kind=cron
// is validated at create time and the first planned_at is derived
// from the expression (no initial_planned_at override). "0 9 * * 1" =
// next Monday 09:00 UTC.
func TestCreate_CronAcceptedAndPlannedDerived(t *testing.T) {
	ext, store, _, _ := withWiredExtension(t)
	owner := newFakeState("ses-cron-create")

	before := time.Now().UTC()
	body := callTool(t, ext, owner, "create", map[string]any{
		"kind":          schedstore.KindWake,
		"schedule_kind": schedstore.ScheduleCron,
		"schedule_spec": "0 9 * * 1",
		"name":          "weekly report",
		"wake_message":  "ping",
	})
	out := decodeCreate(t, body)
	if out.Status != schedstore.StatusActive {
		t.Fatalf("status=%q, want active (body: %s)", out.Status, body)
	}

	planned, err := time.Parse(time.RFC3339Nano, out.InitialPlannedAt)
	if err != nil {
		t.Fatalf("planned_at not RFC3339: %v (%q)", err, out.InitialPlannedAt)
	}
	// Derived instant must match runner.Cron.Next evaluated around the
	// same wall clock — minute granularity makes before/after agree
	// unless we straddle the exact fire minute (not "next Monday 9am").
	sched, _ := runner.Cron("0 9 * * 1", time.UTC)
	lo := sched.Next(before)
	hi := sched.Next(time.Now().UTC())
	if !planned.Equal(lo) && !planned.Equal(hi) {
		t.Fatalf("planned_at=%v, want %v (or %v)", planned, lo, hi)
	}
	if planned.Hour() != 9 || planned.Minute() != 0 || planned.Weekday() != time.Monday {
		t.Errorf("derived instant %v is not Monday 09:00", planned)
	}

	// Row persisted with the cron schedule_kind + spec.
	row, err := store.GetTask(context.Background(), out.TaskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if row.ScheduleKind != schedstore.ScheduleCron {
		t.Errorf("schedule_kind=%q, want cron", row.ScheduleKind)
	}
	if row.Spec.ScheduleSpec != "0 9 * * 1" {
		t.Errorf("schedule_spec=%q", row.Spec.ScheduleSpec)
	}
}

// TestCreate_CronTimezonePersisted confirms a valid IANA timezone is
// accepted + persisted onto the spec for the recompute path to reuse.
func TestCreate_CronTimezonePersisted(t *testing.T) {
	ext, store, _, _ := withWiredExtension(t)
	owner := newFakeState("ses-cron-tz")

	body := callTool(t, ext, owner, "create", map[string]any{
		"kind":          schedstore.KindWake,
		"schedule_kind": schedstore.ScheduleCron,
		"schedule_spec": "0 9 * * *",
		"timezone":      "Europe/Berlin",
		"name":          "daily",
		"wake_message":  "ping",
	})
	out := decodeCreate(t, body)
	row, err := store.GetTask(context.Background(), out.TaskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if row.Spec.Timezone != "Europe/Berlin" {
		t.Errorf("timezone=%q, want Europe/Berlin", row.Spec.Timezone)
	}
}

// TestCreate_CronBadExprRejected asserts an unparseable cron spec is
// rejected at create with invalid_args and no row written.
func TestCreate_CronBadExprRejected(t *testing.T) {
	ext, _, _, _ := withWiredExtension(t)
	owner := newFakeState("ses-cron-bad")

	body := callTool(t, ext, owner, "create", map[string]any{
		"kind":          schedstore.KindWake,
		"schedule_kind": schedstore.ScheduleCron,
		"schedule_spec": "not a cron",
		"name":          "broken",
		"wake_message":  "ping",
	})
	if got := decodeErr(t, body).Code; got != "invalid_args" {
		t.Fatalf("error code=%q, want invalid_args (body: %s)", got, body)
	}
}

// TestCreate_CronBadTimezoneRejected asserts an unknown IANA zone is
// rejected at create with invalid_args.
func TestCreate_CronBadTimezoneRejected(t *testing.T) {
	ext, _, _, _ := withWiredExtension(t)
	owner := newFakeState("ses-cron-badtz")

	body := callTool(t, ext, owner, "create", map[string]any{
		"kind":          schedstore.KindWake,
		"schedule_kind": schedstore.ScheduleCron,
		"schedule_spec": "0 9 * * *",
		"timezone":      "Mars/Phobos",
		"name":          "badtz",
		"wake_message":  "ping",
	})
	if got := decodeErr(t, body).Code; got != "invalid_args" {
		t.Fatalf("error code=%q, want invalid_args (body: %s)", got, body)
	}
}

// TestCreate_CronBadExprRejectedEvenWithOverride asserts the cron
// expression is validated at create even when initial_planned_at is
// supplied — that override path skips planned-at derivation but must
// not let a broken recurrence spec through to pause on first fire.
func TestCreate_CronBadExprRejectedEvenWithOverride(t *testing.T) {
	ext, _, _, _ := withWiredExtension(t)
	owner := newFakeState("ses-cron-override-bad")

	body := callTool(t, ext, owner, "create", map[string]any{
		"kind":               schedstore.KindWake,
		"schedule_kind":      schedstore.ScheduleCron,
		"schedule_spec":      "not a cron",
		"initial_planned_at": time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano),
		"name":               "override-bad",
		"wake_message":       "ping",
	})
	if got := decodeErr(t, body).Code; got != "invalid_args" {
		t.Fatalf("error code=%q, want invalid_args (body: %s)", got, body)
	}
}

func decodeCreate(t *testing.T, body json.RawMessage) createOutput {
	t.Helper()
	var out createOutput
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode create: %v\nbody: %s", err, body)
	}
	return out
}

// jsonUnmarshalForTest is a compact wrapper that keeps the
// create-output decode tidy without polluting the per-test import
// list further.
func jsonUnmarshalForTest(b []byte, v any) error {
	return json.Unmarshal(b, v)
}
