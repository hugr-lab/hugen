package tool

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// Reconnector retries dead MCP providers in the background and
// notifies registered callbacks on recovery. It is owned by
// ToolManager: every MCPProvider added through AddProvider gets a
// stale-hook that registers it here when the provider transitions to
// stale (typically after a synchronous maybeReconnect attempt failed).
//
// The loop runs one ticker shared across all tracked providers.
// Per-provider state (backoff, attempt count, next-due time) lives
// on the *reconnectTarget. Tick scans the target map, picks the ones
// whose nextTry has elapsed, and runs Reconnect with a per-attempt
// timeout. A successful reconnect untracks the provider, fires every
// OnRecover callback, and the provider's own ProviderHealthChanged
// {Healthy} event surfaces upstream through its Subscribe channel.
//
// The "store the reconnect callback on the provider" pattern, instead
// of subscribing to every provider's event stream from the
// reconnector, keeps the wiring trivial: the provider knows when it
// went stale; it tells the reconnector. No extra goroutine per
// provider, no race between Subscribe registration and the first
// stale event.
type Reconnector struct {
	log *slog.Logger

	// Backoff schedule. minBackoff is the first delay between attempts;
	// each failure doubles up to maxBackoff. Defaults: 5s → 10min.
	minBackoff time.Duration
	maxBackoff time.Duration

	// tickInterval drives the scanning loop. Tests use a short value.
	tickInterval time.Duration
	// attemptTimeout caps a single Reconnect call so a hung dial
	// doesn't block the loop. Default 30s.
	attemptTimeout time.Duration

	mu        sync.Mutex
	targets   map[string]*reconnectTarget
	callbacks []func(name string)

	loopMu     sync.Mutex
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	loopActive bool
}

// reconnectTarget is the per-provider state held by the loop.
type reconnectTarget struct {
	provider *MCPProvider
	backoff  time.Duration
	nextTry  time.Time
	attempts int
}

// ReconnectorOption configures a freshly-built Reconnector.
type ReconnectorOption func(*Reconnector)

// WithReconnectBackoff overrides the (min, max) backoff schedule.
// Used by tests to drive shorter cycles.
func WithReconnectBackoff(min, max time.Duration) ReconnectorOption {
	return func(r *Reconnector) {
		if min > 0 {
			r.minBackoff = min
		}
		if max > 0 {
			r.maxBackoff = max
		}
	}
}

// WithReconnectTickInterval sets the scan frequency. Tests use a
// short value (e.g. 10ms) so the suite finishes quickly; production
// uses the default 1s.
func WithReconnectTickInterval(d time.Duration) ReconnectorOption {
	return func(r *Reconnector) {
		if d > 0 {
			r.tickInterval = d
		}
	}
}

// WithReconnectAttemptTimeout caps how long a single Reconnect call
// can run before the loop gives up and re-schedules with backoff.
func WithReconnectAttemptTimeout(d time.Duration) ReconnectorOption {
	return func(r *Reconnector) {
		if d > 0 {
			r.attemptTimeout = d
		}
	}
}

// NewReconnector constructs a Reconnector with the default backoff
// schedule (5s → 10min, 1s tick, 30s per-attempt timeout). Apply
// options to override.
func NewReconnector(log *slog.Logger, opts ...ReconnectorOption) *Reconnector {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	r := &Reconnector{
		log:            log,
		minBackoff:     5 * time.Second,
		maxBackoff:     10 * time.Minute,
		tickInterval:   1 * time.Second,
		attemptTimeout: 30 * time.Second,
		targets:        make(map[string]*reconnectTarget),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Start launches the scan loop. Idempotent — second call after a
// successful Start is a no-op. ctx cancellation stops the loop.
func (r *Reconnector) Start(ctx context.Context) {
	r.loopMu.Lock()
	defer r.loopMu.Unlock()
	if r.loopActive {
		return
	}
	loopCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	r.loopActive = true
	r.wg.Add(1)
	go r.loop(loopCtx)
}

// Stop signals the loop to exit and waits for the goroutine to
// finish. Idempotent.
func (r *Reconnector) Stop() {
	r.loopMu.Lock()
	cancel := r.cancel
	active := r.loopActive
	r.loopActive = false
	r.cancel = nil
	r.loopMu.Unlock()
	if !active || cancel == nil {
		return
	}
	cancel()
	r.wg.Wait()
}

// Track marks a provider as stale and schedules the first reconnect
// attempt. Idempotent — re-Track of an already-tracked provider
// keeps its existing backoff/attempt state. Called by the provider's
// own stale-hook.
func (r *Reconnector) Track(p *MCPProvider) {
	if p == nil {
		return
	}
	name := p.Name()
	if name == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.targets[name]; ok {
		return
	}
	r.targets[name] = &reconnectTarget{
		provider: p,
		backoff:  r.minBackoff,
		nextTry:  time.Now().Add(r.minBackoff),
	}
	r.log.Info("mcp reconnect: tracking provider",
		"provider", name, "first_attempt_in", r.minBackoff)
}

// Forget drops a provider from tracking. Used by ToolManager.
// RemoveProvider so a removed provider doesn't keep being retried.
// Idempotent.
func (r *Reconnector) Forget(name string) {
	if name == "" {
		return
	}
	r.mu.Lock()
	delete(r.targets, name)
	r.mu.Unlock()
}

// OnRecover registers a callback fired (in the loop's own goroutine)
// every time a tracked provider successfully reconnects. Multiple
// callbacks are supported and called in registration order.
//
// Typical wiring: cmd/hugen wires this to a session-manager
// broadcast that pushes a system_marker{subject:"mcp_recovered"}
// into every live root session's inbox so the model sees the
// recovery in its transcript.
func (r *Reconnector) OnRecover(fn func(name string)) {
	if fn == nil {
		return
	}
	r.mu.Lock()
	r.callbacks = append(r.callbacks, fn)
	r.mu.Unlock()
}

// Tracking returns the names of providers currently in the
// retry queue. Sorted; nil-safe; empty when the queue is clean.
// Used by tests + future status surfaces.
func (r *Reconnector) Tracking() []string {
	r.mu.Lock()
	out := make([]string, 0, len(r.targets))
	for name := range r.targets {
		out = append(out, name)
	}
	r.mu.Unlock()
	sort.Strings(out)
	return out
}

func (r *Reconnector) loop(ctx context.Context) {
	defer r.wg.Done()
	t := time.NewTicker(r.tickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.tick(ctx)
		}
	}
}

// tick scans every target whose nextTry has elapsed and runs one
// Reconnect attempt against it. On success: untrack + fire callbacks.
// On failure: bump attempt counter, double backoff (capped), reset
// nextTry. Each attempt runs under attemptTimeout so a hung dial
// doesn't block the rest of the loop.
func (r *Reconnector) tick(ctx context.Context) {
	now := time.Now()
	r.mu.Lock()
	due := make([]*reconnectTarget, 0, len(r.targets))
	for _, target := range r.targets {
		if !target.nextTry.After(now) {
			due = append(due, target)
		}
	}
	callbacks := append([]func(string){}, r.callbacks...)
	r.mu.Unlock()

	for _, target := range due {
		if target.provider == nil {
			continue
		}
		// Provider may have been Closed externally between Track and
		// tick — skip and untrack so we don't keep retrying a dead
		// object.
		if target.provider.IsClosed() {
			r.Forget(target.provider.Name())
			continue
		}
		// Skip if the provider isn't actually stale anymore (e.g. a
		// concurrent in-line maybeReconnect succeeded between ticks).
		if !target.provider.IsStale() {
			r.Forget(target.provider.Name())
			continue
		}
		target.attempts++
		attemptCtx, cancel := context.WithTimeout(ctx, r.attemptTimeout)
		err := target.provider.Reconnect(attemptCtx)
		cancel()

		if err == nil {
			r.log.Info("mcp reconnect: provider recovered",
				"provider", target.provider.Name(),
				"attempts", target.attempts)
			r.Forget(target.provider.Name())
			for _, fn := range callbacks {
				fn(target.provider.Name())
			}
			continue
		}
		// Failure path: exponential backoff up to cap.
		r.mu.Lock()
		target.backoff *= 2
		if target.backoff > r.maxBackoff {
			target.backoff = r.maxBackoff
		}
		target.nextTry = time.Now().Add(target.backoff)
		r.mu.Unlock()
		// Persistent retries surface as warn — operators reading the
		// log see one provider stuck in a loop without it spamming
		// every tick (only-on-attempt cadence, capped at 10min).
		r.log.Warn("mcp reconnect: attempt failed",
			"provider", target.provider.Name(),
			"attempt", target.attempts,
			"next_in", target.backoff,
			"err", err)
	}
}
