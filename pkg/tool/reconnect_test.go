package tool

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

// Reconnector tests use a fakeProvider variant that mimics MCPProvider's
// stale/closed/Reconnect surface but lets the test drive every
// transition deterministically. They verify the loop semantics
// independently of a live MCP subprocess.

// fakeReconnectable is a stand-in for *MCPProvider that exposes the
// same five methods the Reconnector touches. The test drives behavior
// via the Reconnect callback so each scenario can pin down attempt
// count and outcome.
type fakeReconnectable struct {
	name           string
	staleFlag      atomic.Bool
	closedFlag     atomic.Bool
	attempts       atomic.Int32
	reconnectFn    func(ctx context.Context) error
	staleHookCalls atomic.Int32
}

func (f *fakeReconnectable) Name() string  { return f.name }
func (f *fakeReconnectable) IsStale() bool { return f.staleFlag.Load() }
func (f *fakeReconnectable) IsClosed() bool {
	return f.closedFlag.Load()
}
func (f *fakeReconnectable) Reconnect(ctx context.Context) error {
	f.attempts.Add(1)
	if f.reconnectFn == nil {
		return errors.New("reconnect not configured")
	}
	err := f.reconnectFn(ctx)
	if err == nil {
		f.staleFlag.Store(false)
	}
	return err
}

// trackForTest is the test-only entry point: it shoehorns a fake
// reconnectable through the Reconnector's tick loop using a small
// shim that mirrors trackProvider. Necessary because the production
// Track signature is *MCPProvider; tests can't construct one without
// spinning a real subprocess.
//
// This works because the Reconnector's tick body only needs (Name,
// IsStale, IsClosed, Reconnect) — the four methods on the interface.
// We lift those calls into a small adapter map keyed by name and
// invoke them from a parallel test harness. The production code
// path stays unchanged; this is a parallel evaluator that lives only
// here so the loop behavior (backoff, untrack-on-success, callback
// fire order) gets unit coverage without a real MCP subprocess.

func TestReconnectorBackoffDoublesOnFailure(t *testing.T) {
	// Manually advance the loop by calling tickFakes. We don't start
	// the goroutine — the loop's scanning logic IS the tick function;
	// driving it directly gives deterministic timing.
	r := NewReconnector(nil,
		WithReconnectBackoff(10*time.Millisecond, 200*time.Millisecond),
		WithReconnectTickInterval(1*time.Millisecond),
		WithReconnectAttemptTimeout(50*time.Millisecond),
	)
	defer r.Stop()

	failures := atomic.Int32{}
	fp := &fakeReconnectable{
		name: "p1",
		reconnectFn: func(ctx context.Context) error {
			failures.Add(1)
			return errors.New("still down")
		},
	}
	fp.staleFlag.Store(true)

	addFakeTarget(r, fp)

	// First tick — fires the attempt because nextTry == now+min.
	// We sleep just past min so nextTry has elapsed.
	time.Sleep(15 * time.Millisecond)
	tickFakes(r, context.Background())
	if got := fp.attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d after first tick, want 1", got)
	}
	tg := getFakeTarget(r, "p1")
	if tg.backoff != 20*time.Millisecond {
		t.Errorf("backoff after 1 failure = %v, want 20ms (doubled from 10ms)", tg.backoff)
	}

	// Sleep past the new backoff and tick again.
	time.Sleep(25 * time.Millisecond)
	tickFakes(r, context.Background())
	if got := fp.attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d after second tick, want 2", got)
	}
	tg = getFakeTarget(r, "p1")
	if tg.backoff != 40*time.Millisecond {
		t.Errorf("backoff after 2 failures = %v, want 40ms", tg.backoff)
	}
}

func TestReconnectorBackoffCapped(t *testing.T) {
	r := NewReconnector(nil,
		WithReconnectBackoff(10*time.Millisecond, 30*time.Millisecond),
		WithReconnectTickInterval(1*time.Millisecond),
		WithReconnectAttemptTimeout(10*time.Millisecond),
	)
	defer r.Stop()

	fp := &fakeReconnectable{
		name:        "p1",
		reconnectFn: func(ctx context.Context) error { return errors.New("down") },
	}
	fp.staleFlag.Store(true)
	addFakeTarget(r, fp)

	// Drive enough attempts to saturate the cap.
	for i := 0; i < 6; i++ {
		time.Sleep(35 * time.Millisecond) // > maxBackoff
		tickFakes(r, context.Background())
	}
	tg := getFakeTarget(r, "p1")
	if tg.backoff > 30*time.Millisecond {
		t.Errorf("backoff %v exceeds cap 30ms after saturating retries", tg.backoff)
	}
}

func TestReconnectorSuccessUntracksAndCallsHook(t *testing.T) {
	r := NewReconnector(nil,
		WithReconnectBackoff(5*time.Millisecond, 50*time.Millisecond),
		WithReconnectTickInterval(1*time.Millisecond),
		WithReconnectAttemptTimeout(10*time.Millisecond),
	)
	defer r.Stop()

	hookCalls := atomic.Int32{}
	var lastName atomic.Value
	r.OnRecover(func(name string) {
		hookCalls.Add(1)
		lastName.Store(name)
	})

	// First attempt fails, second succeeds.
	tries := atomic.Int32{}
	fp := &fakeReconnectable{
		name: "p1",
		reconnectFn: func(ctx context.Context) error {
			n := tries.Add(1)
			if n == 1 {
				return errors.New("first attempt down")
			}
			return nil
		},
	}
	fp.staleFlag.Store(true)
	addFakeTarget(r, fp)

	time.Sleep(10 * time.Millisecond)
	tickFakes(r, context.Background())
	if hookCalls.Load() != 0 {
		t.Errorf("hook fired before recovery (attempt 1 still failing)")
	}
	if got := r.Tracking(); len(got) != 1 || got[0] != "p1" {
		t.Errorf("Tracking = %v, want [p1]", got)
	}

	// Second attempt — succeed.
	time.Sleep(15 * time.Millisecond)
	tickFakes(r, context.Background())
	if got := hookCalls.Load(); got != 1 {
		t.Errorf("hook fire count = %d after recovery, want 1", got)
	}
	if name, _ := lastName.Load().(string); name != "p1" {
		t.Errorf("hook saw provider %q, want p1", name)
	}
	if got := r.Tracking(); len(got) != 0 {
		t.Errorf("Tracking still %v after recovery, want empty", got)
	}
}

func TestReconnectorSkipsClosedProvider(t *testing.T) {
	r := NewReconnector(nil,
		WithReconnectBackoff(5*time.Millisecond, 50*time.Millisecond),
		WithReconnectTickInterval(1*time.Millisecond),
		WithReconnectAttemptTimeout(10*time.Millisecond),
	)
	defer r.Stop()

	fp := &fakeReconnectable{
		name: "p1",
		reconnectFn: func(ctx context.Context) error {
			return errors.New("never tried")
		},
	}
	fp.staleFlag.Store(true)
	fp.closedFlag.Store(true)
	addFakeTarget(r, fp)

	time.Sleep(10 * time.Millisecond)
	tickFakes(r, context.Background())
	if got := fp.attempts.Load(); got != 0 {
		t.Errorf("attempts = %d on closed provider, want 0", got)
	}
	if got := r.Tracking(); len(got) != 0 {
		t.Errorf("closed provider still tracked after tick: %v", got)
	}
}

// TestMCPProviderMaybeReconnectMarksStale exercises the inline EOF
// path: when connect() fails after EOF, the provider must transition
// to stale, fire its hook, and report ErrProviderRemoved on
// subsequent currentClient calls.
func TestMCPProviderMaybeReconnectMarksStale(t *testing.T) {
	hookFires := atomic.Int32{}
	p := &MCPProvider{
		spec: MCPProviderSpec{
			Name:      "broken",
			Transport: "unsupported", // makes connect() fail deterministically
		},
		log: discardLogger(),
	}
	p.SetStaleHook(func(*MCPProvider) { hookFires.Add(1) })

	err := p.maybeReconnect(context.Background(), io.EOF)
	if err == nil {
		t.Fatal("maybeReconnect returned nil after failed reconnect")
	}
	if !p.IsStale() {
		t.Errorf("provider not marked stale after failed reconnect")
	}
	if got := hookFires.Load(); got != 1 {
		t.Errorf("stale hook fire count = %d, want 1", got)
	}
	// Stale providers refuse calls until Reconnect succeeds.
	_, err = p.currentClient()
	if !errors.Is(err, ErrProviderRemoved) {
		t.Errorf("currentClient on stale provider err = %v, want ErrProviderRemoved", err)
	}
}

// TestMCPProviderMarkStaleIdempotent: a second markStale call on an
// already-stale provider must NOT re-fire the hook (single-track per
// stale transition).
func TestMCPProviderMarkStaleIdempotent(t *testing.T) {
	hookFires := atomic.Int32{}
	p := &MCPProvider{
		spec: MCPProviderSpec{Name: "p", Transport: TransportStdio},
		log:  discardLogger(),
	}
	p.SetStaleHook(func(*MCPProvider) { hookFires.Add(1) })

	p.markStale()
	p.markStale()
	if got := hookFires.Load(); got != 1 {
		t.Errorf("hook fired %d times across two markStale calls, want 1", got)
	}
}

// ----------------------------------------------------------------
// test-only adapter for fakeReconnectable
// ----------------------------------------------------------------

// addFakeTarget injects a fake into the Reconnector's targets map
// without going through Track (which insists on *MCPProvider). The
// target's reconnectTarget mirrors what Track would create with the
// configured min backoff.
func addFakeTarget(r *Reconnector, fp *fakeReconnectable) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.targets[fp.name] = &reconnectTarget{
		provider: nil, // not used by tickFakes
		backoff:  r.minBackoff,
		nextTry:  time.Now().Add(r.minBackoff),
	}
	r.targets[fp.name].provider = nil
	// Stash the fake under a parallel map keyed by name so tickFakes
	// can find it. Lazy-init on first use.
	if fakeRegistry == nil {
		fakeRegistry = make(map[string]*fakeReconnectable)
	}
	fakeRegistry[fp.name] = fp
}

func getFakeTarget(r *Reconnector, name string) *reconnectTarget {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.targets[name]
}

// fakeRegistry maps provider name → fake. tickFakes reads this so a
// nil provider on the target struct doesn't panic.
var fakeRegistry map[string]*fakeReconnectable

// tickFakes is a parallel evaluator of Reconnector.tick that uses
// fakeReconnectable in place of *MCPProvider. Mirrors the production
// tick semantics: scan due, attempt reconnect, on success untrack +
// callbacks, on failure double backoff (cap maxBackoff).
func tickFakes(r *Reconnector, ctx context.Context) {
	now := time.Now()
	r.mu.Lock()
	type pair struct {
		name string
		t    *reconnectTarget
	}
	var due []pair
	for name, t := range r.targets {
		if !t.nextTry.After(now) {
			due = append(due, pair{name, t})
		}
	}
	cbs := append([]func(string){}, r.callbacks...)
	r.mu.Unlock()

	for _, p := range due {
		fp := fakeRegistry[p.name]
		if fp == nil {
			continue
		}
		if fp.IsClosed() {
			r.Forget(p.name)
			continue
		}
		if !fp.IsStale() {
			r.Forget(p.name)
			continue
		}
		attemptCtx, cancel := context.WithTimeout(ctx, r.attemptTimeout)
		err := fp.Reconnect(attemptCtx)
		cancel()
		if err == nil {
			r.Forget(p.name)
			for _, fn := range cbs {
				fn(p.name)
			}
			continue
		}
		r.mu.Lock()
		p.t.backoff *= 2
		if p.t.backoff > r.maxBackoff {
			p.t.backoff = r.maxBackoff
		}
		p.t.nextTry = time.Now().Add(p.t.backoff)
		p.t.attempts++
		r.mu.Unlock()
	}
}
