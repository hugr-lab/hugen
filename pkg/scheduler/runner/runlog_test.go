package runner

import (
	"context"
	"testing"
	"time"
)

func TestMemoryRunLogAppendFinalize(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	log := NewMemoryRunLog(0)

	now := time.Unix(1700000000, 0)
	if err := log.Append(ctx, RunLogEntry{
		Name:      "demo",
		FireSeq:   1,
		PlannedAt: now,
		StartedAt: now,
		Status:    RunLogInFlight,
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	if err := log.Finalize(ctx, "demo", 1, RunLogEntry{
		CompletedAt: now.Add(time.Second),
		Duration:    time.Second,
		Status:      RunLogCompleted,
		Summary:     "ok",
	}); err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	entries, err := log.ListByName(ctx, "demo", 0)
	if err != nil {
		t.Fatalf("ListByName: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries: want 1 got %d", len(entries))
	}
	e := entries[0]
	if e.Status != RunLogCompleted || e.Summary != "ok" || e.Duration != time.Second {
		t.Fatalf("entry mismatch: %+v", e)
	}
	// PlannedAt/StartedAt/Name/FireSeq were preserved from Append.
	if e.Name != "demo" || e.FireSeq != 1 || !e.PlannedAt.Equal(now) || !e.StartedAt.Equal(now) {
		t.Fatalf("Append-time fields lost: %+v", e)
	}
}

func TestMemoryRunLogLatestSuccess(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	log := NewMemoryRunLog(0)

	now := time.Unix(1700000000, 0)
	for seq, status := range []RunLogStatus{RunLogCompleted, RunLogFailed, RunLogCompleted, RunLogTimeout} {
		_ = log.Append(ctx, RunLogEntry{
			Name:    "demo",
			FireSeq: seq + 1,
			Status:  status,
			Summary: "s" + string(rune('0'+seq+1)),
		})
		_ = log.Finalize(ctx, "demo", seq+1, RunLogEntry{
			Status:      status,
			CompletedAt: now.Add(time.Duration(seq) * time.Second),
			Summary:     "s" + string(rune('0'+seq+1)),
		})
	}

	got, ok, err := log.LatestSuccess(ctx, "demo")
	if err != nil {
		t.Fatalf("LatestSuccess err: %v", err)
	}
	if !ok {
		t.Fatalf("LatestSuccess returned not-found")
	}
	// seq=3 is the last RunLogCompleted entry.
	if got.FireSeq != 3 {
		t.Fatalf("LatestSuccess seq: want 3 got %d", got.FireSeq)
	}
}

func TestMemoryRunLogCapEvicts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	log := NewMemoryRunLog(3)
	for i := 1; i <= 6; i++ {
		_ = log.Append(ctx, RunLogEntry{Name: "demo", FireSeq: i, Status: RunLogCompleted})
	}
	entries, err := log.ListByName(ctx, "demo", 0)
	if err != nil {
		t.Fatalf("ListByName: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("cap should evict to 3, got %d", len(entries))
	}
	// Oldest evicted; tail preserved.
	if entries[0].FireSeq != 4 || entries[2].FireSeq != 6 {
		t.Fatalf("retained wrong window: %+v", entries)
	}
}
