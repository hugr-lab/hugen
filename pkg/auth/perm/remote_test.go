package perm

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeQuerier is a stub Querier with controllable rule outputs
// and per-call hooks (fail-next-call, simulate-slow).
type fakeQuerier struct {
	mu       sync.Mutex
	rules    []Rule
	calls    atomic.Int64
	failNext int
	delay    time.Duration
	failErr  error
}

func newFakeQuerier(rules []Rule) *fakeQuerier {
	return &fakeQuerier{rules: rules}
}

func (q *fakeQuerier) QueryRules(ctx context.Context) ([]Rule, error) {
	q.calls.Add(1)
	if q.delay > 0 {
		select {
		case <-time.After(q.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.failNext > 0 {
		q.failNext--
		return nil, q.failErr
	}
	out := make([]Rule, len(q.rules))
	copy(out, q.rules)
	return out, nil
}

func (q *fakeQuerier) SetRules(rules []Rule) {
	q.mu.Lock()
	q.rules = rules
	q.mu.Unlock()
}

func (q *fakeQuerier) FailNext(n int, err error) {
	q.mu.Lock()
	q.failNext = n
	q.failErr = err
	q.mu.Unlock()
}

func TestRemotePermissions_InitialFetchOnFirstResolve(t *testing.T) {
	cfg := &fakeView{refreshInterval: time.Minute}
	q := newFakeQuerier([]Rule{
		{Type: "T", Field: "f", Disabled: true},
	})
	r := NewRemotePermissions(cfg, fakeIdentity{id: "ag01"}, q)

	got, err := r.Resolve(context.Background(), "T", "f")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !got.Disabled {
		t.Errorf("Disabled = false; want remote rule to apply")
	}
	if !got.FromRemote {
		t.Errorf("FromRemote = false")
	}
	if q.calls.Load() != 1 {
		t.Errorf("QueryRules calls = %d, want 1", q.calls.Load())
	}
}

func TestRemotePermissions_ConfigFloorWinsOnDisable(t *testing.T) {
	cfg := &fakeView{
		refreshInterval: time.Minute,
		rules:           []Rule{{Type: "T", Field: "f", Disabled: true}},
	}
	// Remote says NOT disabled (no rule); the floor still wins.
	q := newFakeQuerier(nil)
	r := NewRemotePermissions(cfg, fakeIdentity{id: "ag01"}, q)

	got, err := r.Resolve(context.Background(), "T", "f")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !got.Disabled {
		t.Errorf("Disabled = false; floor must win")
	}
	if !got.FromConfig {
		t.Errorf("FromConfig = false")
	}
}

func TestRemotePermissions_TTLRefresh(t *testing.T) {
	cfg := &fakeView{refreshInterval: 50 * time.Millisecond}
	q := newFakeQuerier([]Rule{
		{Type: "T", Field: "f", Disabled: false},
	})
	r := NewRemotePermissions(cfg, fakeIdentity{id: "ag01"}, q)

	if _, err := r.Resolve(context.Background(), "T", "f"); err != nil {
		t.Fatal(err)
	}
	if q.calls.Load() != 1 {
		t.Fatalf("after first resolve calls = %d", q.calls.Load())
	}
	// Update remote rules; before TTL elapses Resolve still
	// returns the cached snapshot.
	q.SetRules([]Rule{{Type: "T", Field: "f", Disabled: true}})
	got, _ := r.Resolve(context.Background(), "T", "f")
	if got.Disabled {
		t.Errorf("Disabled flipped before TTL elapsed")
	}
	time.Sleep(60 * time.Millisecond)
	got, _ = r.Resolve(context.Background(), "T", "f")
	if !got.Disabled {
		t.Errorf("Disabled = false; want refresh to have picked up new rule")
	}
}

func TestRemotePermissions_SingleflightCollapsesConcurrent(t *testing.T) {
	cfg := &fakeView{refreshInterval: time.Hour}
	q := newFakeQuerier([]Rule{{Type: "T", Field: "f"}})
	q.delay = 50 * time.Millisecond
	r := NewRemotePermissions(cfg, fakeIdentity{id: "ag01"}, q)

	// Drive 8 concurrent first-resolves; only one fetch should
	// hit the querier (singleflight collapse).
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = r.Resolve(context.Background(), "T", "f")
		}()
	}
	wg.Wait()
	if got := q.calls.Load(); got != 1 {
		t.Errorf("QueryRules calls = %d, want 1 (singleflight collapse)", got)
	}
}

func TestRemotePermissions_RefreshFailurePreservesSnapshot(t *testing.T) {
	cfg := &fakeView{refreshInterval: 30 * time.Millisecond}
	q := newFakeQuerier([]Rule{{Type: "T", Field: "f", Disabled: true}})
	r := NewRemotePermissions(cfg, fakeIdentity{id: "ag01"}, q)

	if _, err := r.Resolve(context.Background(), "T", "f"); err != nil {
		t.Fatal(err)
	}
	// Force the next refresh to fail.
	q.FailNext(99, errors.New("hugr unreachable"))
	time.Sleep(40 * time.Millisecond) // age > TTL

	got, err := r.Resolve(context.Background(), "T", "f")
	if err != nil {
		t.Fatalf("Resolve: %v (want preserved snapshot)", err)
	}
	if !got.Disabled {
		t.Errorf("Disabled = false; cached snapshot must be reused on refresh failure")
	}
}

func TestRemotePermissions_HardExpiryAfter3xTTL(t *testing.T) {
	cfg := &fakeView{refreshInterval: 20 * time.Millisecond}
	q := newFakeQuerier([]Rule{{Type: "T", Field: "f"}})
	r := NewRemotePermissions(cfg, fakeIdentity{id: "ag01"}, q)
	r.hardExpiryMult = 2 // shorter for the test (2× = 40 ms)

	if _, err := r.Resolve(context.Background(), "T", "f"); err != nil {
		t.Fatal(err)
	}
	q.FailNext(99, errors.New("hugr down"))
	time.Sleep(50 * time.Millisecond) // age > 2× TTL

	if _, err := r.Resolve(context.Background(), "T", "f"); !errors.Is(err, ErrSnapshotStale) {
		t.Errorf("err = %v, want ErrSnapshotStale", err)
	}
}

func TestRemotePermissions_RefreshTriggersImmediateFetch(t *testing.T) {
	cfg := &fakeView{refreshInterval: time.Hour}
	q := newFakeQuerier([]Rule{{Type: "T", Field: "f"}})
	r := NewRemotePermissions(cfg, fakeIdentity{id: "ag01"}, q)

	if _, err := r.Resolve(context.Background(), "T", "f"); err != nil {
		t.Fatal(err)
	}
	// Tier-2 ruleset changes upstream.
	q.SetRules([]Rule{{Type: "T", Field: "f", Disabled: true}})
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if q.calls.Load() < 2 {
		t.Errorf("QueryRules calls = %d, want ≥ 2 after Refresh", q.calls.Load())
	}
	got, err := r.Resolve(context.Background(), "T", "f")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Disabled {
		t.Errorf("Refresh did not pick up new rules")
	}
}

func TestRemotePermissions_SubscribeReceivesEvents(t *testing.T) {
	cfg := &fakeView{refreshInterval: time.Hour}
	q := newFakeQuerier([]Rule{{Type: "T", Field: "f"}})
	r := NewRemotePermissions(cfg, fakeIdentity{id: "ag01"}, q)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := r.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := r.Resolve(context.Background(), "T", "f"); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-ch:
		if ev.Err != nil {
			t.Errorf("event.Err = %v", ev.Err)
		}
	case <-time.After(time.Second):
		t.Fatalf("missing RefreshEvent on initial fetch")
	}

	// Failure event flows out too.
	q.FailNext(1, errors.New("boom"))
	if err := r.Refresh(context.Background()); err == nil {
		t.Errorf("expected refresh error")
	}
	select {
	case ev := <-ch:
		if ev.Err == nil {
			t.Errorf("missing failure event")
		}
	case <-time.After(time.Second):
		t.Fatalf("no RefreshEvent on failure")
	}
}

func TestRemotePermissions_AgentIDForwarded(t *testing.T) {
	cfg := &fakeView{refreshInterval: time.Hour}
	q := newFakeQuerier(nil)
	r := NewRemotePermissions(cfg, fakeIdentity{id: "ag01"}, q)
	if got := r.AgentID(); got != "ag01" {
		t.Errorf("AgentID = %q, want ag01", got)
	}
}
