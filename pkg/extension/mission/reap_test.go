package mission_test

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/hugr-lab/hugen/pkg/extension/mission"
	"github.com/hugr-lab/hugen/pkg/internal/fixture"
	"github.com/hugr-lab/hugen/pkg/scheduler/runner"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

// reapStore filters fixture.TestStore.ListSessions by status (the
// underlying fixture returns everything regardless of args).
type reapStore struct {
	*fixture.TestStore
}

func (s *reapStore) ListSessions(ctx context.Context, agentID, status string) ([]store.SessionRow, error) {
	rows, err := s.TestStore.ListSessions(ctx, agentID, status)
	if err != nil {
		return nil, err
	}
	out := rows[:0]
	for _, r := range rows {
		if agentID != "" && r.AgentID != agentID {
			continue
		}
		if status != "" && r.Status != status {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

const agentID = "agent-1"

func openRow(t *testing.T, st *reapStore, row store.SessionRow) {
	t.Helper()
	if err := st.OpenSession(context.Background(), row); err != nil {
		t.Fatalf("OpenSession %q: %v", row.ID, err)
	}
}

func TestReapOrphanSubagents_TerminatesChildOfTerminatedParent(t *testing.T) {
	t.Parallel()
	st := &reapStore{TestStore: fixture.NewTestStore()}

	openRow(t, st, store.SessionRow{ID: "parent", AgentID: agentID, SessionType: "root", Status: store.StatusTerminated})
	openRow(t, st, store.SessionRow{ID: "child", AgentID: agentID, SessionType: "subagent", ParentSessionID: "parent", Status: store.StatusActive})
	openRow(t, st, store.SessionRow{ID: "live-parent", AgentID: agentID, SessionType: "root", Status: store.StatusActive})
	openRow(t, st, store.SessionRow{ID: "live-child", AgentID: agentID, SessionType: "subagent", ParentSessionID: "live-parent", Status: store.StatusActive})

	fn := mission.ReapOrphanSubagents(agentID, st, discardLogger())
	if _, err := fn(context.Background(), runner.FireMeta{Name: "test"}); err != nil {
		t.Fatalf("reap err: %v", err)
	}

	if got, _ := st.LoadSession(context.Background(), "child"); got.Status != store.StatusTerminated {
		t.Fatalf("orphan child should be terminated, got %s", got.Status)
	}
	if got, _ := st.LoadSession(context.Background(), "live-child"); got.Status != store.StatusActive {
		t.Fatalf("live child should remain active, got %s", got.Status)
	}
	if got, _ := st.LoadSession(context.Background(), "parent"); got.Status != store.StatusTerminated {
		t.Fatalf("parent row should remain terminated, got %s", got.Status)
	}
}

func TestReapOrphanSubagents_RootRowsIgnored(t *testing.T) {
	t.Parallel()
	st := &reapStore{TestStore: fixture.NewTestStore()}
	openRow(t, st, store.SessionRow{ID: "lonely-root", AgentID: agentID, SessionType: "root", Status: store.StatusActive})

	fn := mission.ReapOrphanSubagents(agentID, st, discardLogger())
	if _, err := fn(context.Background(), runner.FireMeta{Name: "test"}); err != nil {
		t.Fatalf("reap err: %v", err)
	}
	if got, _ := st.LoadSession(context.Background(), "lonely-root"); got.Status != store.StatusActive {
		t.Fatalf("root row should not be touched, got %s", got.Status)
	}
}

func TestReapOrphanSubagents_ChildWithLiveParentIgnored(t *testing.T) {
	t.Parallel()
	st := &reapStore{TestStore: fixture.NewTestStore()}
	openRow(t, st, store.SessionRow{ID: "p", AgentID: agentID, SessionType: "root", Status: store.StatusActive})
	openRow(t, st, store.SessionRow{ID: "c", AgentID: agentID, SessionType: "subagent", ParentSessionID: "p", Status: store.StatusActive})

	fn := mission.ReapOrphanSubagents(agentID, st, discardLogger())
	if _, err := fn(context.Background(), runner.FireMeta{Name: "test"}); err != nil {
		t.Fatalf("reap err: %v", err)
	}
	if got, _ := st.LoadSession(context.Background(), "c"); got.Status != store.StatusActive {
		t.Fatalf("child of live parent must stay active, got %s", got.Status)
	}
}

func TestReapOrphanSubagents_NoActive(t *testing.T) {
	t.Parallel()
	st := &reapStore{TestStore: fixture.NewTestStore()}
	fn := mission.ReapOrphanSubagents(agentID, st, discardLogger())
	out, err := fn(context.Background(), runner.FireMeta{Name: "test"})
	if err != nil {
		t.Fatalf("reap err: %v", err)
	}
	if out.Summary != "no active sessions" {
		t.Fatalf("summary: %q", out.Summary)
	}
}
