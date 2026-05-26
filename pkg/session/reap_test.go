package session_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/internal/fixture"
	"github.com/hugr-lab/hugen/pkg/scheduler/runner"
	"github.com/hugr-lab/hugen/pkg/session"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

// reapStore wraps fixture.TestStore so ListSessions respects the
// status filter the reaper passes. The base TestStore returns
// every row regardless of agentID / status; the reaper would
// short-circuit on terminated rows it should never see.
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

func newReapStore() *reapStore {
	return &reapStore{TestStore: fixture.NewTestStore()}
}

type liveSet []string

func (l liveSet) SessionsLive() []string { return []string(l) }

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

const agentID = "agent-1"

func openRow(t *testing.T, st *reapStore, row store.SessionRow) {
	t.Helper()
	if err := st.OpenSession(context.Background(), row); err != nil {
		t.Fatalf("OpenSession %q: %v", row.ID, err)
	}
}

func TestReapProcessOrphans_TerminatesStaleNonLive(t *testing.T) {
	t.Parallel()
	st := newReapStore()
	openRow(t, st, store.SessionRow{ID: "stale", AgentID: agentID, SessionType: "root", Status: store.StatusActive})
	openRow(t, st, store.SessionRow{ID: "fresh", AgentID: agentID, SessionType: "root", Status: store.StatusActive})
	openRow(t, st, store.SessionRow{ID: "live", AgentID: agentID, SessionType: "root", Status: store.StatusActive})

	// "fresh" has a recent event → event-freshness guard protects it.
	// "stale" appends only an OLD event → cutoff filters it out, reaper sees none.
	// "live" needs no events — the liveSet oracle protects it
	// regardless of activity.
	if err := st.AppendEvent(context.Background(), store.EventRow{
		ID: "ev-fresh", SessionID: "fresh", AgentID: agentID,
		EventType: "user_message", CreatedAt: time.Now(),
	}, ""); err != nil {
		t.Fatalf("AppendEvent fresh: %v", err)
	}
	if err := st.AppendEvent(context.Background(), store.EventRow{
		ID: "ev-stale", SessionID: "stale", AgentID: agentID,
		EventType: "user_message", CreatedAt: time.Now().Add(-2 * time.Hour),
	}, ""); err != nil {
		t.Fatalf("AppendEvent stale: %v", err)
	}

	fn := session.ReapProcessOrphans(agentID, st, liveSet{"live"}, discardLogger())
	outcome, err := fn(context.Background(), runner.FireMeta{Name: "test"})
	if err != nil {
		t.Fatalf("reap err: %v", err)
	}
	if got, err := st.LoadSession(context.Background(), "stale"); err != nil || got.Status != store.StatusTerminated {
		t.Fatalf("stale row: status=%s err=%v", got.Status, err)
	}
	if got, err := st.LoadSession(context.Background(), "fresh"); err != nil || got.Status != store.StatusActive {
		t.Fatalf("fresh row should remain active: status=%s err=%v", got.Status, err)
	}
	if got, err := st.LoadSession(context.Background(), "live"); err != nil || got.Status != store.StatusActive {
		t.Fatalf("live row should remain active: status=%s err=%v", got.Status, err)
	}
	if outcome.Summary == "" {
		t.Fatalf("expected outcome summary, got empty")
	}
}

func TestReapProcessOrphans_NoActive(t *testing.T) {
	t.Parallel()
	st := newReapStore()
	fn := session.ReapProcessOrphans(agentID, st, liveSet{}, discardLogger())
	outcome, err := fn(context.Background(), runner.FireMeta{Name: "test"})
	if err != nil {
		t.Fatalf("reap err: %v", err)
	}
	if outcome.Summary != "no active sessions" {
		t.Fatalf("summary: %q", outcome.Summary)
	}
}

func TestReapProcessOrphans_NilStore(t *testing.T) {
	t.Parallel()
	fn := session.ReapProcessOrphans(agentID, nil, liveSet{}, discardLogger())
	if _, err := fn(context.Background(), runner.FireMeta{}); err == nil {
		t.Fatalf("expected error on nil store")
	}
}
