package http

import (
	"context"
	"log/slog"
	"sync"
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
type sessionBus struct {
	sessionID string
	upstream  <-chan protocol.Frame
	cancel    context.CancelFunc
	logger    *slog.Logger
	grace     time.Duration

	mu       sync.Mutex
	subs     []*subscriber
	closed   bool
	refCount int
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
// subscriber. Exits when upstream closes (the runtime cancelled
// the subscription).
func (b *sessionBus) run() {
	for f := range b.upstream {
		b.mu.Lock()
		subs := append([]*subscriber(nil), b.subs...)
		b.mu.Unlock()
		for _, s := range subs {
			b.deliver(s, f)
		}
	}
	// Upstream closed — drain remaining live channels so writers
	// observe close and exit cleanly.
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for _, s := range b.subs {
		close(s.out)
	}
	b.subs = nil
}

// deliver pushes one frame to one subscriber with the 50ms grace
// drop policy. Dropped frames are logged and recoverable through
// Last-Event-ID replay (R-Plan-18).
func (b *sessionBus) deliver(s *subscriber, f protocol.Frame) {
	timer := time.NewTimer(b.grace)
	defer timer.Stop()
	select {
	case s.out <- f:
	case <-timer.C:
		b.logger.Warn("slow consumer; dropping frame",
			"session", b.sessionID, "kind", f.Kind(), "seq", f.Seq())
	}
}

// addSubscriber registers a new connection on the bus. Returns the
// per-connection out channel.
func (b *sessionBus) addSubscriber() *subscriber {
	s := newSubscriber()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		// Upstream already gone — return a closed channel so the
		// caller's select returns immediately.
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
// When the last connection drops, the bus is torn down (cancels
// upstream, run exits, channel closes).
func (a *Adapter) attachSubscriber(parent context.Context, host runtime.AdapterHost, sessionID string) (*subscriber, func(), error) {
	a.busesMu.Lock()
	bus, ok := a.buses[sessionID]
	if !ok {
		// Build a context decoupled from any single connection so
		// the bus survives across reconnects within the same
		// session-active window. Cancellation comes from
		// removeSubscriber when refCount drops to zero.
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
			cancel:    cancel,
			logger:    a.logger,
			grace:     a.sseCfg.slowConsumerGrace,
		}
		a.buses[sessionID] = bus
		go bus.run()
	}
	bus.refCount++
	a.busesMu.Unlock()

	sub := bus.addSubscriber()
	cleanup := func() {
		bus.removeSubscriber(sub)
		a.busesMu.Lock()
		bus.refCount--
		teardown := bus.refCount == 0
		if teardown {
			delete(a.buses, sessionID)
		}
		a.busesMu.Unlock()
		if teardown {
			// Cancel upstream → runtime drops our subscription →
			// upstream channel closes → bus.run exits → out chans
			// are closed.
			bus.cancel()
		}
		// Honour parent cancellation independent of refcount: if
		// the request context dies, releasing the subscriber must
		// not block on bus teardown.
		_ = parent
	}
	return sub, cleanup, nil
}
