package http

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/runtime"
)

// sessionBus is the per-session fan-out point inside the http
// adapter. It Subscribes upstream on the runtime exactly once and
// re-fans-out frames to its connection-level subscribers, applying
// the 50ms slow-consumer grace from sse-wire-format.md.
//
// The runtime's adapterHost.Subscribe gives us one channel per
// Subscribe call; the runtime fan-out is non-blocking with no
// grace. Doing the timed-drop here means the adapter — not the
// runtime — owns the backpressure policy. Per-connection drops
// don't affect persistence (the frame is in session_events) or
// other connections (they have their own buffered channels).
//
// Lifecycle:
//   - First connection on a session creates the bus, starts the
//     fan-out goroutine, and Subscribe-s upstream.
//   - Subsequent connections refcount in.
//   - When the last connection drops, refCount→0 triggers
//     bus.cancel(); busCtx fires; bus.run exits via the
//     ctx.Done() arm of its select; the runtime drops the
//     channel from its subscriber list. The channel itself is
//     not closed here — Runtime.Shutdown owns that on process
//     exit so we don't double-close.
type sessionBus struct {
	sessionID string
	upstream  <-chan protocol.Frame
	ctx       context.Context
	cancel    context.CancelFunc
	logger    *slog.Logger
	grace     time.Duration
	parent    *Adapter // back-ref so shutdown can deregister

	mu       sync.Mutex
	subs     []*subscriber
	closed   bool
	refCount int

	// drops counts how many frames the slow-consumer policy dropped
	// across all subscribers. Tests assert this is non-zero so the
	// policy is exercised, not just the liveness of fast consumers.
	drops atomic.Int64
}

// subscriber is one SSE connection on a session.
type subscriber struct {
	// Buffered to absorb bursts; the 50ms grace forgives momentary
	// pauses, and beyond that the frame is dropped to this consumer
	// only. The contract caps capacity at 64.
	out chan protocol.Frame
}

func newSubscriber() *subscriber {
	return &subscriber{out: make(chan protocol.Frame, 64)}
}

// run reads upstream frames and fans them out to every active
// subscriber. Exits via either (a) the bus context being cancelled
// (refCount→0 teardown) or (b) the upstream channel closing
// (Runtime.Shutdown). Both paths converge on closing the
// per-connection channels so writers observe end-of-stream.
//
// `defer b.cancel()` ensures the upstream-close path also tears
// down busCtx; otherwise the context.WithCancel propagator
// goroutine lingers until the bus is GC'd.
func (b *sessionBus) run() {
	defer b.shutdown()
	defer b.cancel()
	for {
		select {
		case <-b.ctx.Done():
			return
		case f, ok := <-b.upstream:
			if !ok {
				return
			}
			b.mu.Lock()
			subs := append([]*subscriber(nil), b.subs...)
			b.mu.Unlock()
			for _, s := range subs {
				b.deliver(s, f)
			}
		}
	}
}

// shutdown closes every per-connection channel, removes the bus
// from the parent's buses map, and marks the bus closed so a late
// addSubscriber can short-circuit. Idempotent via b.closed.
//
// Lock order: parent.busesMu → b.mu (matches attachSubscriber's
// order so the two paths can't deadlock).
func (b *sessionBus) shutdown() {
	b.parent.busesMu.Lock()
	if existing, ok := b.parent.buses[b.sessionID]; ok && existing == b {
		delete(b.parent.buses, b.sessionID)
	}
	b.parent.busesMu.Unlock()

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for _, s := range b.subs {
		close(s.out)
	}
	b.subs = nil
}

// graceTimerPool reuses *time.Timer instances across deliver calls
// so a burst of frames doesn't allocate one timer per (sub, frame).
// Reset semantics: take from pool, Reset(grace), use, Stop+drain
// before returning.
var graceTimerPool = sync.Pool{
	New: func() any {
		// New(time.Hour) starts the timer in a stopped-ish state;
		// Reset before use replaces the deadline.
		t := time.NewTimer(time.Hour)
		if !t.Stop() {
			<-t.C
		}
		return t
	},
}

// deliver pushes one frame to one subscriber with the slow-consumer
// drop grace. Dropped frames are logged and recoverable through
// Last-Event-ID replay (R-Plan-18).
func (b *sessionBus) deliver(s *subscriber, f protocol.Frame) {
	timer := graceTimerPool.Get().(*time.Timer)
	timer.Reset(b.grace)
	select {
	case s.out <- f:
		// Send won — stop the timer and drain its channel if it
		// fired between our select and Stop.
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	case <-timer.C:
		b.drops.Add(1)
		b.logger.Warn("slow consumer; dropping frame",
			"session", b.sessionID, "kind", f.Kind(), "seq", f.Seq())
	}
	graceTimerPool.Put(timer)
}

// dropCount returns the cumulative number of frames the bus has
// dropped to slow consumers. Tests use it to assert the drop policy
// actually fires; production callers don't read it (use the slog
// warning instead).
func (b *sessionBus) dropCount() int64 { return b.drops.Load() }

// busDrops returns the drop count for a session's bus, or 0 when
// no bus exists. Test-only accessor on the adapter; production code
// has no reason to peek here.
func (a *Adapter) busDrops(sessionID string) int64 {
	a.busesMu.Lock()
	defer a.busesMu.Unlock()
	if b, ok := a.buses[sessionID]; ok {
		return b.dropCount()
	}
	return 0
}

// addSubscriber registers a new connection on the bus. Returns the
// per-connection out channel. If the bus is shutting down (busCtx
// already cancelled — covers both the upstream-closed and the
// refCount→0 paths), addSubscriber returns a closed channel so the
// caller's select returns immediately. Checking ctx.Err() instead
// of a separate `closed` bool keeps the two states from drifting
// (cancel may fire moments before shutdown sets b.closed=true).
func (b *sessionBus) addSubscriber() *subscriber {
	s := newSubscriber()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ctx.Err() != nil || b.closed {
		close(s.out)
		return s
	}
	b.subs = append(b.subs, s)
	return s
}

// removeSubscriber drops a connection from the bus. Decrements the
// refCount under the parent adapter's lock — the parent decides
// whether to tear the bus down entirely.
func (b *sessionBus) removeSubscriber(s *subscriber) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	out := b.subs[:0]
	for _, x := range b.subs {
		if x != s {
			out = append(out, x)
		}
	}
	b.subs = out
}

// attachSubscriber gets-or-creates the session's bus and registers
// a per-connection subscriber on it. Returns the subscriber and a
// cleanup func that the caller MUST invoke on disconnect.
//
// The bus is started lazily: the first connection on a session
// performs the upstream Subscribe and spawns the run goroutine.
// When the last connection drops, refCount→0 cancels busCtx and
// the run goroutine deregisters the bus from a.buses.
//
// Atomicity: addSubscriber and refCount mutation both happen under
// busesMu so a concurrent bus.shutdown (which also acquires
// busesMu) cannot race with attach. A bus that is shutting down
// has already deleted itself from a.buses, so the lookup at the
// top creates a fresh bus rather than handing out a dead one.
func (a *Adapter) attachSubscriber(host runtime.AdapterHost, sessionID string) (*subscriber, func(), error) {
	a.busesMu.Lock()
	bus, ok := a.buses[sessionID]
	if !ok {
		// Build a context decoupled from any single connection so
		// the bus survives across reconnects within the same
		// session-active window. Cancellation comes from cleanup
		// when refCount drops to zero, or from Runtime.Shutdown
		// closing the upstream channel.
		busCtx, cancel := context.WithCancel(context.Background())
		live, err := host.Subscribe(busCtx, sessionID)
		if err != nil {
			cancel()
			a.busesMu.Unlock()
			return nil, nil, err
		}
		bus = &sessionBus{
			sessionID: sessionID,
			upstream:  live,
			ctx:       busCtx,
			cancel:    cancel,
			logger:    a.logger,
			grace:     a.sseCfg.slowConsumerGrace,
			parent:    a,
		}
		a.buses[sessionID] = bus
		go bus.run()
	}
	bus.refCount++
	sub := bus.addSubscriber()
	a.busesMu.Unlock()

	cleanup := func() {
		bus.removeSubscriber(sub)
		a.busesMu.Lock()
		bus.refCount--
		teardown := bus.refCount == 0
		if teardown {
			// Deregister BEFORE bus.cancel so a concurrent
			// attachSubscriber can't find this bus while
			// bus.run is still tearing it down. shutdown's own
			// deregister becomes a no-op then.
			if existing, ok := a.buses[sessionID]; ok && existing == bus {
				delete(a.buses, sessionID)
			}
		}
		a.busesMu.Unlock()
		if teardown {
			// Cancel busCtx → bus.run exits via ctx.Done →
			// shutdown() runs, closes per-connection channels.
			// Idempotent with Runtime.Shutdown's path that
			// closes the upstream first.
			bus.cancel()
		}
	}
	return sub, cleanup, nil
}
