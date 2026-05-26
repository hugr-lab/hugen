//go:build duckdb_arrow

package scheduler_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	hugr "github.com/hugr-lab/query-engine"
	"github.com/hugr-lab/query-engine/pkg/auth"
	coredb "github.com/hugr-lab/query-engine/pkg/data-sources/sources/runtime/core-db"
	"github.com/hugr-lab/query-engine/pkg/db"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hugr-lab/hugen/pkg/extension/scheduler"
	"github.com/hugr-lab/hugen/pkg/scheduler/runner"
	schedstore "github.com/hugr-lab/hugen/pkg/scheduler/store"
	"github.com/hugr-lab/hugen/pkg/store/local"
	"github.com/hugr-lab/hugen/pkg/store/local/migrate"
	"github.com/hugr-lab/hugen/pkg/store/queries"
)

// reapTestEnv stands up the same hub.db + Hugr engine as the
// scheduler/store package tests so the reaper drives a real
// TaskStore end-to-end. Phase 6.1b — task_log_reap_stuck body.
func reapTestEnv(t *testing.T) (schedstore.TaskStore, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	hubPath := dir + "/memory.db"

	require.NoError(t, migrate.Ensure(migrate.Config{
		Path:          hubPath,
		VectorSize:    0,
		EmbedderModel: "",
		Seed: &migrate.SeedData{
			AgentType: migrate.SeedAgentType{ID: "test", Name: "test"},
			Agent:     migrate.SeedAgent{ID: "agt_test01", ShortID: "ts01", Name: "test"},
		},
	}))

	source := local.NewSource(local.SourceConfig{
		Path:          hubPath,
		VectorSize:    0,
		EmbedderModel: "",
	})
	svc, err := hugr.New(hugr.Config{
		DB:     db.Config{},
		CoreDB: coredb.New(coredb.Config{VectorSize: 0}),
		Auth:   &auth.Config{},
	})
	require.NoError(t, err)
	require.NoError(t, svc.AttachRuntimeSource(ctx, source))
	require.NoError(t, svc.Init(ctx))
	t.Cleanup(func() { _ = svc.Close() })

	require.NoError(t, queries.RunMutation(ctx, svc,
		`mutation ($data: hub_db_sessions_mut_input_data!) {
			hub { db { agent {
				insert_sessions(data: $data) { id }
			}}}
		}`,
		map[string]any{"data": map[string]any{
			"id":           "ses-owner",
			"agent_id":     "agt_test01",
			"status":       "active",
			"session_type": "root",
		}},
	))

	return schedstore.NewLocalTaskStore(svc), "agt_test01"
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestReapStuckTaskRuns_AppendsCancelledForStuck covers the load-
// bearing post-pivot reaper contract: a `started` row with no
// terminal match past the cutoff gets an INSERT of `cancelled` row
// — append-only, no UPDATE of the original `started` row.
func TestReapStuckTaskRuns_AppendsCancelledForStuck(t *testing.T) {
	st, agentID := reapTestEnv(t)
	ctx := context.Background()

	require.NoError(t, st.OpenTask(ctx, schedstore.TaskRow{
		ID:             "tsk_reap",
		AgentID:        agentID,
		Kind:           schedstore.KindSpawn,
		ScheduleKind:   schedstore.ScheduleCron,
		OwnerSessionID: "ses-owner",
		Spec: schedstore.TaskSpec{
			Name:         "Reap target",
			ScheduleSpec: "0 9 * * *",
			EndCondition: schedstore.TaskEndCondition{Kind: "until_cancel"},
		},
	}, time.Now().UTC().Add(-2*time.Hour)))

	// Backdate via planned_at to put fire 1 firmly past the cutoff.
	twoHoursAgo := time.Now().UTC().Add(-2 * time.Hour)
	require.NoError(t, st.AppendLog(ctx, schedstore.TaskLogEntry{
		TaskID: "tsk_reap", AgentID: agentID, FireSeq: 1,
		EventType: schedstore.LogEventStarted, PlannedAt: twoHoursAgo,
	}))

	// Run the reaper with FireMeta.PlannedAt = now so the cutoff
	// = now - 1h captures the 2h-old started row.
	fn := scheduler.ReapStuckTaskRuns(agentID, st, discardLogger())
	out, err := fn(ctx, runner.FireMeta{Name: "task_log_reap_stuck", PlannedAt: time.Now().UTC()})
	require.NoError(t, err)
	assert.Contains(t, out.Summary, "1")
	assert.Equal(t, "reap_stuck", out.Reason)

	// Verify a cancelled row landed for (task=tsk_reap, fire_seq=1)
	// with outcome.reason=reap_stuck — append-only, the started row
	// stays in place untouched.
	logs, err := st.ListLogByTask(ctx, "tsk_reap", schedstore.ListLogOpts{})
	require.NoError(t, err)

	var startedRowsCount, cancelledRowsCount int
	for _, l := range logs {
		switch l.EventType {
		case schedstore.LogEventStarted:
			startedRowsCount++
		case schedstore.LogEventCancelled:
			cancelledRowsCount++
			require.NotNil(t, l.Outcome)
			assert.Equal(t, "reap_stuck", l.Outcome.Reason)
		}
	}
	assert.Equal(t, 1, startedRowsCount, "started row must remain untouched")
	assert.Equal(t, 1, cancelledRowsCount, "exactly one cancelled row was appended")
}

// TestReapStuckTaskRuns_NoStuckIsBenign verifies the "nothing to do"
// path returns a successful outcome with no log mutation.
func TestReapStuckTaskRuns_NoStuckIsBenign(t *testing.T) {
	st, agentID := reapTestEnv(t)
	fn := scheduler.ReapStuckTaskRuns(agentID, st, discardLogger())
	out, err := fn(context.Background(), runner.FireMeta{Name: "task_log_reap_stuck", PlannedAt: time.Now().UTC()})
	require.NoError(t, err)
	assert.Equal(t, "no stuck fires", out.Summary)
}

// TestReapStuckTaskRuns_NilStore returns an error rather than
// silently swallowing the misconfiguration.
func TestReapStuckTaskRuns_NilStore(t *testing.T) {
	fn := scheduler.ReapStuckTaskRuns("agt", nil, discardLogger())
	_, err := fn(context.Background(), runner.FireMeta{})
	require.Error(t, err)
}
