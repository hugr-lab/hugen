package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeClock returns a tickable now-fn; tests advance it manually so
// they don't sleep waiting for real wall-clock ticks. Safe for
// concurrent reads from the service + writes from the test goroutine.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{now: start}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// awaitFireCount polls the registration's snapshot until FireCount
// reaches want AND the most recent fire's fn body has returned
// (LastFireAt set in recordOutcome AFTER the fn returns). Returns
// the final snapshot for diagnostic.
func awaitFireCount(t *testing.T, s *Service, name string, want int) RunnerStatus {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		st, ok := s.Status(context.Background(), name)
		if !ok {
			t.Fatalf("status %q: not registered", name)
		}
		if st.FireCount >= want && !st.LastFireAt.IsZero() {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("await %q FireCount=%d/want=%d lastFire=%v",
				name, st.FireCount, want, st.LastFireAt)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestServiceRegisterFiresOnStart(t *testing.T) {
	t.Parallel()
	clk := newFakeClock(time.Unix(1700000000, 0))
	svc := New(
		WithLogger(discardLogger()),
		WithClock(clk.Now),
		WithTickInterval(time.Millisecond),
	)

	var fired int32
	if err := svc.Register(context.Background(), "demo",
		Every(time.Hour),
		func(_ context.Context, _ FireMeta) (Outcome, error) {
			atomic.AddInt32(&fired, 1)
			return Outcome{Summary: "ok"}, nil
		},
	); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = svc.Stop(context.Background()) }()

	// Initial tick should NOT fire (next_fire_at = now + 1h).
	time.Sleep(20 * time.Millisecond)
	if got := atomic.LoadInt32(&fired); got != 0 {
		t.Fatalf("fired too eagerly: %d", got)
	}

	// Advance past the schedule.
	clk.Advance(2 * time.Hour)
	awaitFireCount(t, svc, "demo", 1)
	if got := atomic.LoadInt32(&fired); got < 1 {
		t.Fatalf("expected at least 1 fire, got %d", got)
	}
}

func TestServicePauseResume(t *testing.T) {
	t.Parallel()
	clk := newFakeClock(time.Unix(1700000000, 0))
	svc := New(
		WithLogger(discardLogger()),
		WithClock(clk.Now),
		WithTickInterval(time.Millisecond),
	)
	defer func() { _ = svc.Stop(context.Background()) }()

	var fired int32
	if err := svc.Register(context.Background(), "p",
		Every(time.Second),
		func(_ context.Context, _ FireMeta) (Outcome, error) {
			atomic.AddInt32(&fired, 1)
			return Outcome{}, nil
		},
	); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := svc.Pause(context.Background(), "p"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	clk.Advance(10 * time.Second)
	time.Sleep(20 * time.Millisecond)
	if got := atomic.LoadInt32(&fired); got != 0 {
		t.Fatalf("paused fn should not fire, got %d", got)
	}

	if err := svc.Resume(context.Background(), "p"); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	clk.Advance(2 * time.Second)
	awaitFireCount(t, svc, "p", 1)
}

func TestServiceUnregisterStopsFiring(t *testing.T) {
	t.Parallel()
	clk := newFakeClock(time.Unix(1700000000, 0))
	svc := New(
		WithLogger(discardLogger()),
		WithClock(clk.Now),
		WithTickInterval(time.Millisecond),
	)
	defer func() { _ = svc.Stop(context.Background()) }()

	var fired int32
	_ = svc.Register(context.Background(), "u",
		Every(time.Second),
		func(_ context.Context, _ FireMeta) (Outcome, error) {
			atomic.AddInt32(&fired, 1)
			return Outcome{}, nil
		},
	)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	clk.Advance(2 * time.Second)
	awaitFireCount(t, svc, "u", 1)

	if err := svc.Unregister(context.Background(), "u"); err != nil {
		t.Fatalf("Unregister: %v", err)
	}
	before := atomic.LoadInt32(&fired)
	clk.Advance(10 * time.Second)
	time.Sleep(20 * time.Millisecond)
	if got := atomic.LoadInt32(&fired); got != before {
		t.Fatalf("unregistered fn still fires: before=%d after=%d", before, got)
	}
	if _, ok := svc.Status(context.Background(), "u"); ok {
		t.Fatalf("Status returns ok=true for unregistered name")
	}
}

func TestServicePanicIsolated(t *testing.T) {
	t.Parallel()
	clk := newFakeClock(time.Unix(1700000000, 0))
	svc := New(
		WithLogger(discardLogger()),
		WithClock(clk.Now),
		WithTickInterval(time.Millisecond),
	)
	defer func() { _ = svc.Stop(context.Background()) }()

	var goodFired int32
	_ = svc.Register(context.Background(), "bad", Every(time.Second),
		func(_ context.Context, _ FireMeta) (Outcome, error) {
			panic("kaboom")
		},
	)
	_ = svc.Register(context.Background(), "good", Every(time.Second),
		func(_ context.Context, _ FireMeta) (Outcome, error) {
			atomic.AddInt32(&goodFired, 1)
			return Outcome{}, nil
		},
	)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	clk.Advance(2 * time.Second)
	awaitFireCount(t, svc, "good", 1)
	if got := atomic.LoadInt32(&goodFired); got < 1 {
		t.Fatalf("good fn should still fire after sibling panic, got %d", got)
	}

	// Advance again to prove the tick loop survived the panic across
	// a second cycle — the first joint tick only proves goroutine
	// isolation, not loop survival.
	clk.Advance(2 * time.Second)
	awaitFireCount(t, svc, "good", 2)
	if got := atomic.LoadInt32(&goodFired); got < 2 {
		t.Fatalf("good fn should fire twice after two panics, got %d", got)
	}

	st, _ := svc.Status(context.Background(), "bad")
	if st.FireCount < 2 {
		t.Fatalf("bad fn FireCount should re-dispatch after panic recovery: %+v", st)
	}
}

func TestServicePrevOutcomePopulated(t *testing.T) {
	t.Parallel()
	clk := newFakeClock(time.Unix(1700000000, 0))
	svc := New(
		WithLogger(discardLogger()),
		WithClock(clk.Now),
		WithTickInterval(time.Millisecond),
	)
	defer func() { _ = svc.Stop(context.Background()) }()

	var (
		mu       sync.Mutex
		captured []FireMeta
	)
	_ = svc.Register(context.Background(), "p", Every(time.Second),
		func(_ context.Context, fire FireMeta) (Outcome, error) {
			mu.Lock()
			captured = append(captured, fire)
			mu.Unlock()
			return Outcome{Summary: fmt.Sprintf("fire-%d", fire.FireSeq)}, nil
		},
	)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	clk.Advance(2 * time.Second)
	awaitFireCount(t, svc, "p", 1)
	clk.Advance(2 * time.Second)
	awaitFireCount(t, svc, "p", 2)

	mu.Lock()
	defer mu.Unlock()
	if len(captured) < 2 {
		t.Fatalf("captured only %d fires", len(captured))
	}
	if captured[0].PrevOutcome != nil {
		t.Fatalf("first fire's PrevOutcome should be nil, got %+v", captured[0].PrevOutcome)
	}
	if captured[1].PrevOutcome == nil || captured[1].PrevOutcome.Summary != "fire-1" {
		t.Fatalf("second fire's PrevOutcome wrong: %+v", captured[1].PrevOutcome)
	}
}

func TestServiceErrorRecordedInRunLog(t *testing.T) {
	t.Parallel()
	clk := newFakeClock(time.Unix(1700000000, 0))
	log := NewMemoryRunLog(0)
	svc := New(
		WithLogger(discardLogger()),
		WithClock(clk.Now),
		WithTickInterval(time.Millisecond),
		WithRunLog(log),
	)
	defer func() { _ = svc.Stop(context.Background()) }()

	_ = svc.Register(context.Background(), "fail", Every(time.Second),
		func(_ context.Context, _ FireMeta) (Outcome, error) {
			return Outcome{}, errors.New("boom")
		},
	)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	clk.Advance(2 * time.Second)
	awaitFireCount(t, svc, "fail", 1)

	// Give the fn goroutine a beat to Finalize the log entry.
	time.Sleep(20 * time.Millisecond)
	entries, _ := log.ListByName(context.Background(), "fail", 0)
	if len(entries) != 1 {
		t.Fatalf("run-log entries: want 1 got %d", len(entries))
	}
	if entries[0].Status != RunLogFailed {
		t.Fatalf("status: want %q got %q", RunLogFailed, entries[0].Status)
	}
	if entries[0].ErrorMessage == "" {
		t.Fatalf("error message empty")
	}
}

func TestServiceListByPrefix(t *testing.T) {
	t.Parallel()
	svc := New(WithLogger(discardLogger()))
	for _, n := range []string{"a", "b_one", "b_two", "c"} {
		_ = svc.Register(context.Background(), n, Every(time.Hour),
			func(_ context.Context, _ FireMeta) (Outcome, error) { return Outcome{}, nil })
	}
	got := svc.ListByPrefix("b_")
	if len(got) != 2 {
		t.Fatalf("ListByPrefix len: got %d want 2 (%+v)", len(got), got)
	}
	if got[0].Name != "b_one" || got[1].Name != "b_two" {
		t.Fatalf("ListByPrefix not sorted: %+v", got)
	}
}

func TestServiceStartPausedSkipsTick(t *testing.T) {
	t.Parallel()
	clk := newFakeClock(time.Unix(1700000000, 0))
	svc := New(
		WithLogger(discardLogger()),
		WithClock(clk.Now),
		WithTickInterval(time.Millisecond),
	)
	defer func() { _ = svc.Stop(context.Background()) }()

	var fired int32
	_ = svc.Register(context.Background(), "dorm", Every(time.Second),
		func(_ context.Context, _ FireMeta) (Outcome, error) {
			atomic.AddInt32(&fired, 1)
			return Outcome{}, nil
		},
		WithStartPaused(),
	)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	clk.Advance(10 * time.Second)
	time.Sleep(20 * time.Millisecond)
	if got := atomic.LoadInt32(&fired); got != 0 {
		t.Fatalf("WithStartPaused: fn should not fire, got %d", got)
	}
}

func TestServiceRegisterValidation(t *testing.T) {
	t.Parallel()
	svc := New(WithLogger(discardLogger()))
	cases := []struct {
		name  string
		sched Schedule
		fn    RunnerFn
	}{
		{name: "", sched: Every(time.Hour), fn: func(_ context.Context, _ FireMeta) (Outcome, error) { return Outcome{}, nil }},
		{name: "x", sched: nil, fn: func(_ context.Context, _ FireMeta) (Outcome, error) { return Outcome{}, nil }},
		{name: "x", sched: Every(time.Hour), fn: nil},
	}
	for _, c := range cases {
		if err := svc.Register(context.Background(), c.name, c.sched, c.fn); err == nil {
			t.Fatalf("Register with name=%q sched=%v fn=%v should error",
				c.name, c.sched, c.fn)
		}
	}
}
