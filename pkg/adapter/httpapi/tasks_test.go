package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/session"
	schedstore "github.com/hugr-lab/hugen/pkg/scheduler/store"
)

// fakeTaskStore is a canned taskReader: rows returned verbatim, next-fire from
// the planned map.
type fakeTaskStore struct {
	rows    []schedstore.TaskRow
	planned map[string]*schedstore.TaskLogEntry
	counts  map[string]int
	gotSess string
}

func (f *fakeTaskStore) ListTasksBySession(_ context.Context, sessionID string, _ schedstore.ListTasksOpts) ([]schedstore.TaskRow, error) {
	f.gotSess = sessionID
	return f.rows, nil
}

func (f *fakeTaskStore) LatestPlannedFire(_ context.Context, taskID string) (*schedstore.TaskLogEntry, error) {
	return f.planned[taskID], nil
}

func (f *fakeTaskStore) CountTasksBySession(_ context.Context, _ string) (map[string]int, error) {
	return f.counts, nil
}

// tasksAdapter wires an allow-open adapter over a fakeHost + optional taskReader,
// owning session "ses-mine".
func tasksAdapter(t *testing.T, host *fakeHost, ts taskReader) *http.ServeMux {
	t.Helper()
	a := New(WithLogger(quietLogger()))
	a.host = host
	a.lifecycleCtx = context.Background()
	a.taskStore = ts
	mux := http.NewServeMux()
	if err := a.mount(mux, false); err != nil {
		t.Fatalf("mount: %v", err)
	}
	return mux
}

func TestListSessionTasks(t *testing.T) {
	planned := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	host := &fakeHost{sessions: []session.SessionSummary{ownedSummary("ses-mine", "active", "local")}}
	ts := &fakeTaskStore{
		rows: []schedstore.TaskRow{{
			ID:           "task-1",
			Kind:         schedstore.KindSpawn,
			Status:       schedstore.StatusActive,
			ScheduleKind: schedstore.ScheduleCron,
			Spec:         schedstore.TaskSpec{Name: "daily", ScheduleSpec: "0 9 * * *"},
		}},
		planned: map[string]*schedstore.TaskLogEntry{"task-1": {PlannedAt: planned}},
		counts:  map[string]int{"active": 1, "cancelled": 2, "completed": 1},
	}
	mux := tasksAdapter(t, host, ts)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/sessions/ses-mine/tasks", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body)
	}
	var resp sessionTasksResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Tasks) != 1 {
		t.Fatalf("tasks = %+v, want 1 row", resp.Tasks)
	}
	got := resp.Tasks[0]
	if got.ID != "task-1" || got.Name != "daily" || got.ScheduleSpec != "0 9 * * *" || got.Status != "active" {
		t.Errorf("row = %+v", got)
	}
	if got.NextFire == nil || !got.NextFire.Equal(planned) {
		t.Errorf("next_fire = %v, want %v", got.NextFire, planned)
	}
	// The owner passed to the store is the path session id.
	if ts.gotSess != "ses-mine" {
		t.Errorf("store queried session %q, want ses-mine", ts.gotSess)
	}
	// archived = cancelled + completed.
	if resp.ArchivedCount != 3 {
		t.Errorf("archived_count = %d, want 3", resp.ArchivedCount)
	}
}

func TestListSessionTasks_EmptyWhenNoStore(t *testing.T) {
	host := &fakeHost{sessions: []session.SessionSummary{ownedSummary("ses-mine", "active", "local")}}
	mux := tasksAdapter(t, host, nil) // scheduler not wired

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/sessions/ses-mine/tasks", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	var resp sessionTasksResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Tasks) != 0 {
		t.Errorf("want empty list, got %+v", resp.Tasks)
	}
}

func TestListSessionTasks_NotOwned(t *testing.T) {
	host := &fakeHost{sessions: []session.SessionSummary{ownedSummary("ses-mine", "active", "local")}}
	mux := tasksAdapter(t, host, &fakeTaskStore{})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/sessions/ses-other/tasks", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404 for a session the caller does not own", rec.Code)
	}
}
