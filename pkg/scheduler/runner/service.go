package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"
)

// Service is the in-process [Runner] implementation. Builders wire
// it once per agent in [pkg/runtime/build.go] and inject it into
// every consumer that needs to schedule work; the same Service
// dispatches reapers (Phase 6.1a), user tasks (Phase 6.1b), and
// future memory-pipeline runs (Phase 7).
type Service struct {
	log          *slog.Logger
	runLog       RunnerRunLog
	tickInterval time.Duration
	nowFn        func() time.Time

	mu      sync.RWMutex
	regs    map[string]*registration
	started bool
	stopped bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	// fireWG tracks in-flight fn goroutines so Stop can wait for
	// them. Kept separate from wg (which tracks the tick loop
	// itself) so Stop can drain in two stages.
	fireWG sync.WaitGroup
}

// Option configures the Service at construction. The variadic
// surface keeps the boot-time call site readable while letting
// tests inject a fake clock + faster tick.
type Option func(*Service)

// WithRunLog overrides the default in-memory run-log. Phase 6.1b
// will pass a persistent backing.
func WithRunLog(l RunnerRunLog) Option {
	return func(s *Service) {
		if l != nil {
			s.runLog = l
		}
	}
}

// WithTickInterval overrides [DefaultTickInterval]. Test-only;
// production callers should leave this at the default.
func WithTickInterval(d time.Duration) Option {
	return func(s *Service) {
		if d > 0 {
			s.tickInterval = d
		}
	}
}

// WithClock injects a clock function. Test-only; production
// callers should leave this at time.Now.
func WithClock(fn func() time.Time) Option {
	return func(s *Service) {
		if fn != nil {
			s.nowFn = fn
		}
	}
}

// WithLogger swaps the structured logger. Defaults to
// slog.Default(). The Service logs at Warn for fire failures and
// Debug for tick decisions.
func WithLogger(l *slog.Logger) Option {
	return func(s *Service) {
		if l != nil {
			s.log = l
		}
	}
}

// New constructs a Service. The returned instance is registered-
// against-and-stopped state until [Service.Start] runs; consumers
// can Register fns either before or after Start (both paths fire
// correctly on the next tick).
func New(opts ...Option) *Service {
	s := &Service{
		log:          slog.Default(),
		runLog:       NewMemoryRunLog(0),
		tickInterval: DefaultTickInterval,
		nowFn:        time.Now,
		regs:         make(map[string]*registration),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// registration is the per-name internal state. The struct is
// pointer-stored in regs so callers like Pause/Resume that hold
// the map read lock can lock the entry without copying.
type registration struct {
	mu          sync.Mutex
	name        string
	sched       Schedule
	fn          RunnerFn
	opts        registerOptions
	paused      bool
	nextFireAt  time.Time
	lastFireAt  time.Time
	lastOutcome *Outcome
	fireCount   int
	inFlight    bool
}

// Register implements [Runner.Register].
func (s *Service) Register(ctx context.Context, name string, sched Schedule, fn RunnerFn, opts ...RegisterOption) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if name == "" {
		return errors.New("runner: Register requires name")
	}
	if sched == nil {
		return errors.New("runner: Register requires schedule")
	}
	if fn == nil {
		return errors.New("runner: Register requires fn")
	}

	o := registerOptions{timeout: DefaultFireTimeout}
	for _, apply := range opts {
		apply(&o)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return errors.New("runner: Register on stopped service")
	}
	now := s.nowFn()
	next := sched.Next(now)
	// WithInitialFireAt overrides the schedule-derived instant so an
	// already-overdue plan instant is preserved (and fires next tick)
	// rather than dropped by a past-rejecting Schedule.Next.
	if !o.initialFireAt.IsZero() {
		next = o.initialFireAt
	}
	// Seed fireCount so the first prepareFire (fireCount++ → seq) reports
	// WithInitialFireSeq(n) as FireSeq n. Zero (the default) preserves the
	// historical seq=1 first fire.
	seedCount := 0
	if o.initialFireSeq > 1 {
		seedCount = o.initialFireSeq - 1
	}
	s.regs[name] = &registration{
		name:       name,
		sched:      sched,
		fn:         fn,
		opts:       o,
		paused:     o.startPaused,
		nextFireAt: next,
		fireCount:  seedCount,
	}
	return nil
}

// Unregister implements [Runner.Unregister].
func (s *Service) Unregister(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.regs, name)
	return nil
}

// Pause implements [Runner.Pause].
func (s *Service) Pause(_ context.Context, name string) error {
	reg, ok := s.lookup(name)
	if !ok {
		return fmt.Errorf("runner: Pause unknown name %q", name)
	}
	reg.mu.Lock()
	reg.paused = true
	reg.mu.Unlock()
	return nil
}

// Resume implements [Runner.Resume].
func (s *Service) Resume(_ context.Context, name string) error {
	reg, ok := s.lookup(name)
	if !ok {
		return fmt.Errorf("runner: Resume unknown name %q", name)
	}
	reg.mu.Lock()
	reg.paused = false
	// Re-anchor the next fire to now so a long pause doesn't
	// trigger an immediate burst of catch-up fires.
	reg.nextFireAt = reg.sched.Next(s.nowFn())
	reg.mu.Unlock()
	return nil
}

// Reschedule implements [Runner.Reschedule]. Sets the registration's
// next fire instant directly without re-creating it — fireCount and
// the in-flight bit are preserved. A past `at` fires on the next tick
// (overdue catch-up); the zero time disarms until the next call.
func (s *Service) Reschedule(_ context.Context, name string, at time.Time) error {
	reg, ok := s.lookup(name)
	if !ok {
		return nil
	}
	reg.mu.Lock()
	reg.nextFireAt = at
	reg.mu.Unlock()
	return nil
}

// Status implements [Runner.Status].
func (s *Service) Status(_ context.Context, name string) (RunnerStatus, bool) {
	reg, ok := s.lookup(name)
	if !ok {
		return RunnerStatus{}, false
	}
	return reg.snapshot(), true
}

// ListByPrefix implements [Runner.ListByPrefix].
func (s *Service) ListByPrefix(prefix string) []RegisteredFn {
	s.mu.RLock()
	regs := make([]*registration, 0, len(s.regs))
	for _, r := range s.regs {
		if prefix == "" || strings.HasPrefix(r.name, prefix) {
			regs = append(regs, r)
		}
	}
	s.mu.RUnlock()

	out := make([]RegisteredFn, 0, len(regs))
	for _, r := range regs {
		out = append(out, RegisteredFn{Name: r.name, Status: r.snapshot()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Start implements [Runner.Start].
func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return nil
	}
	if s.stopped {
		s.mu.Unlock()
		return errors.New("runner: Start on stopped service")
	}
	s.started = true
	// Derive the tick loop's ctx from the caller so external
	// cancellation (e.g. an errgroup unwinding around the runtime)
	// also stops the runner. Stop's own cancel() short-circuits the
	// same goroutine.
	loopCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.mu.Unlock()

	s.wg.Add(1)
	go s.tickLoop(loopCtx)
	s.log.Debug("runner: started", "tick", s.tickInterval)
	return nil
}

// Stop implements [Runner.Stop]. Cancels the tick loop, waits for
// it to exit, then waits for any in-flight fires to drain.
func (s *Service) Stop(ctx context.Context) error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil
	}
	s.stopped = true
	cancel := s.cancel
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	// Wait for the tick loop first so no new fires kick off.
	s.wg.Wait()

	// Drain in-flight fires with a bounded wait honoring ctx so
	// shutdown can cap the cost of a stuck reaper.
	done := make(chan struct{})
	go func() {
		s.fireWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) lookup(name string) (*registration, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.regs[name]
	return r, ok
}

func (s *Service) tickLoop(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(s.tickInterval)
	defer ticker.Stop()
	// Fire once at startup so consumers (and tests) don't have to
	// wait a full tick interval for the first dispatch.
	s.tickOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tickOnce(ctx)
		}
	}
}

func (s *Service) tickOnce(ctx context.Context) {
	now := s.nowFn()
	s.mu.RLock()
	regs := make([]*registration, 0, len(s.regs))
	for _, r := range s.regs {
		regs = append(regs, r)
	}
	s.mu.RUnlock()

	for _, r := range regs {
		if !r.shouldFire(now) {
			continue
		}
		seq, planned, fn, timeout, prev := r.prepareFire(s.nowFn)
		if fn == nil {
			continue
		}
		s.fireWG.Add(1)
		go s.runFire(ctx, r, seq, planned, fn, timeout, prev)
	}
}

func (s *Service) runFire(parent context.Context, r *registration, seq int, planned time.Time, fn RunnerFn, timeout time.Duration, prev *Outcome) {
	defer s.fireWG.Done()
	defer r.markIdle()

	started := s.nowFn()
	if err := s.runLog.Append(context.Background(), RunLogEntry{
		Name:      r.name,
		FireSeq:   seq,
		PlannedAt: planned,
		StartedAt: started,
		Status:    RunLogInFlight,
	}); err != nil {
		s.log.Warn("runner: append run-log (in-flight) failed", "task", r.name, "seq", seq, "err", err)
	}

	ctx := parent
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(parent, timeout)
	}

	outcome, fnErr := s.invoke(ctx, fn, FireMeta{
		Name:        r.name,
		FireSeq:     seq,
		PlannedAt:   planned,
		PrevOutcome: prev,
	})
	if cancel != nil {
		cancel()
	}

	completed := s.nowFn()
	entry := RunLogEntry{
		Name:        r.name,
		FireSeq:     seq,
		PlannedAt:   planned,
		StartedAt:   started,
		CompletedAt: completed,
		Duration:    completed.Sub(started),
		Summary:     outcome.Summary,
	}
	switch {
	case fnErr != nil && errors.Is(fnErr, context.DeadlineExceeded):
		entry.Status = RunLogTimeout
		entry.ErrorMessage = fnErr.Error()
	case fnErr != nil:
		entry.Status = RunLogFailed
		entry.ErrorMessage = fnErr.Error()
	default:
		entry.Status = RunLogCompleted
	}
	if outcome.ErrorMessage != "" && entry.ErrorMessage == "" {
		entry.ErrorMessage = outcome.ErrorMessage
	}
	_ = s.runLog.Finalize(context.Background(), r.name, seq, entry)

	r.recordOutcome(completed, outcome, entry.Status == RunLogCompleted)

	if fnErr != nil {
		s.log.Warn("runner fire failed",
			"name", r.name,
			"seq", seq,
			"status", entry.Status,
			"err", fnErr,
		)
	}
}

// invoke wraps the user fn in a panic-recover so a misbehaving
// registration cannot take down the tick goroutine or sibling
// fires.
func (s *Service) invoke(ctx context.Context, fn RunnerFn, fire FireMeta) (out Outcome, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("runner: fn panic: %v", r)
			s.log.Error("runner: panic in fn",
				"name", fire.Name,
				"seq", fire.FireSeq,
				"panic", r,
				"stack", string(debug.Stack()),
			)
		}
	}()
	return fn(ctx, fire)
}

// shouldFire reports whether the registration's next planned fire
// has arrived and no prior fire is still running. Read-only — no
// state mutation; the caller then races to prepareFire which
// atomically stamps the in-flight bit.
func (r *registration) shouldFire(now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.paused || r.inFlight {
		return false
	}
	if r.nextFireAt.IsZero() {
		return false
	}
	return !r.nextFireAt.After(now)
}

// prepareFire atomically stamps the registration as in-flight,
// advances next_fire_at, increments fire_count, and returns the
// fields the fire goroutine needs to dispatch. Returns nil fn if
// another tick raced ahead — the caller should skip.
func (r *registration) prepareFire(nowFn func() time.Time) (seq int, planned time.Time, fn RunnerFn, timeout time.Duration, prev *Outcome) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.paused || r.inFlight || r.nextFireAt.IsZero() {
		return 0, time.Time{}, nil, 0, nil
	}
	now := nowFn()
	if r.nextFireAt.After(now) {
		return 0, time.Time{}, nil, 0, nil
	}
	r.inFlight = true
	r.fireCount++
	seq = r.fireCount
	planned = r.nextFireAt
	r.nextFireAt = r.sched.Next(now)
	fn = r.fn
	timeout = r.opts.timeout
	prev = r.lastOutcome
	return
}

func (r *registration) markIdle() {
	r.mu.Lock()
	r.inFlight = false
	r.mu.Unlock()
}

func (r *registration) recordOutcome(at time.Time, o Outcome, success bool) {
	r.mu.Lock()
	r.lastFireAt = at
	if success {
		copy := o
		r.lastOutcome = &copy
	}
	r.mu.Unlock()
}

func (r *registration) snapshot() RunnerStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := RunnerStatus{
		Paused:     r.paused,
		NextFireAt: r.nextFireAt,
		LastFireAt: r.lastFireAt,
		FireCount:  r.fireCount,
	}
	if r.lastOutcome != nil {
		copy := *r.lastOutcome
		s.LastOutcome = &copy
	}
	return s
}
