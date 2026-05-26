package scheduler

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	schedstore "github.com/hugr-lab/hugen/pkg/scheduler/store"
)

// TestCreate_RegistersWithRunner asserts the load-bearing 6.1c
// invariant: `task:create` not only persists the row + initial
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

// jsonUnmarshalForTest is a compact wrapper that keeps the
// create-output decode tidy without polluting the per-test import
// list further.
func jsonUnmarshalForTest(b []byte, v any) error {
	return json.Unmarshal(b, v)
}
