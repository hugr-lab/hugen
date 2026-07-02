// runtime.go: supervisor goroutine + Adapter contract. The Runtime
// owns the Manager, runs every adapter under one errgroup, and brokers
// Frame traffic between adapters and Sessions.
//
// Phase 1 ships a single Adapter (console) and a single Agent.
// Sub-agents, peer groups, and remote adapters are later phases.
package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session"
	"github.com/hugr-lab/hugen/pkg/session/store"
)

// Adapter is the surface the runtime exposes to inbound channels
// (console, sse, a2a, ...). The interface lives here, next to its
// consumer, per constitution principle III.
type Adapter interface {
	Name() string
	Run(ctx context.Context, host AdapterHost) error
}

// AdapterHost is the runtime side of the Adapter contract. Adapters
// open/resume sessions and submit/subscribe to Frames through this.
type AdapterHost interface {
	OpenSession(ctx context.Context, req session.OpenRequest) (*session.Session, time.Time, error)
	ResumeSession(ctx context.Context, id string) (*session.Session, error)
	Submit(ctx context.Context, frame protocol.Frame) error
	Subscribe(ctx context.Context, sessionID string) (<-chan protocol.Frame, error)
	CloseSession(ctx context.Context, id, reason string) (time.Time, error)
	ListSessions(ctx context.Context, status string) ([]session.SessionSummary, error)
	// SessionStats returns the persisted event count for sessionID.
	// Phase 5.1c S2 — TUI footer indicator.
	SessionStats(ctx context.Context, sessionID string) (int, error)
	// ListEvents returns events from the session's persisted log.
	// Phase 5.1c — feeds the TUI adapter's on-attach replay
	// (last 100 events stitched into the chat viewport before the
	// next live frame arrives).
	ListEvents(ctx context.Context, sessionID string, opts store.ListEventsOpts) ([]store.EventRow, error)
	Logger() *slog.Logger
}

// Runtime is the supervisor.
type Runtime struct {
	manager  *Manager
	adapters []Adapter
	logger   *slog.Logger

	subMu       sync.Mutex
	subscribers map[string][]chan protocol.Frame

	// ctx is captured at Start() entry and cancelled when the
	// errgroup unwinds (adapter exit or external shutdown). The
	// fanout uses it to bail out of an otherwise blocking send so
	// no goroutine deadlocks on a subscriber that won't drain
	// during teardown.
	ctx context.Context
}

// NewRuntime constructs the supervisor. Adapters are started by Start.
//
// ctx defaults to [context.Background] so [fanoutSend] (called from
// session-pump goroutines) has a non-nil escape hatch even if
// fanout fires before [Runtime.Start] swaps in the errgroup ctx.
// The Start path overwrites this field once, before any adapter
// goroutine launches; the swap is safe because session pumps are
// created by adapters (which can only run via Start).
func NewRuntime(manager *Manager, adapters []Adapter, logger *slog.Logger) *Runtime {
	if logger == nil {
		logger = slog.Default()
	}
	return &Runtime{
		manager:     manager,
		adapters:    adapters,
		logger:      logger,
		subscribers: make(map[string][]chan protocol.Frame),
		ctx:         context.Background(),
	}
}

// Manager exposes the underlying Manager (used by main.go for
// boot-time resume).
func (r *Runtime) Manager() *Manager { return r.manager }

// Start runs every adapter under one errgroup and blocks until ctx
// is done or one of the adapters errors.
func (r *Runtime) Start(ctx context.Context) error {
	g, gctx := errgroup.WithContext(ctx)
	r.ctx = gctx
	for _, a := range r.adapters {
		adapter := a
		host := &adapterHost{rt: r, ctx: gctx}
		g.Go(func() error {
			r.logger.Info("adapter started", "adapter", adapter.Name())
			err := adapter.Run(gctx, host)
			if err != nil && !errors.Is(err, context.Canceled) {
				return fmt.Errorf("adapter %s: %w", adapter.Name(), err)
			}
			return nil
		})
	}
	return g.Wait()
}

// Shutdown is called externally to suspend live sessions; cancellation
// of the parent ctx unblocks Start. Safe to call multiple times.
func (r *Runtime) Shutdown(ctx context.Context) error {
	r.manager.Stop(ctx)
	r.subMu.Lock()
	for _, chans := range r.subscribers {
		for _, c := range chans {
			close(c)
		}
	}
	r.subscribers = make(map[string][]chan protocol.Frame)
	r.subMu.Unlock()
	return nil
}

// fanout pushes a Frame to every subscriber of its session.
//
// Concurrency: a snapshot of the subscriber slice is taken under
// subMu; the actual sends happen unlocked so the send loop never
// holds subMu. Each per-channel send blocks until the subscriber
// drains or the runtime ctx cancels — backpressure flows from the
// adapter through the session pump to [Session.emit] so streaming
// deltas cannot be silently dropped under burst (e.g., a chatty
// LLM emitting 100+ BPE-token deltas per turn against a TUI whose
// per-frame render briefly stalls). A warn log fires if any send
// blocks longer than [fanoutWarnAfter] to make a stuck subscriber
// observable; the wait then continues — drops only happen on ctx
// cancellation (runtime teardown).
func (r *Runtime) fanout(f protocol.Frame) {
	r.subMu.Lock()
	chans := append([]chan protocol.Frame(nil), r.subscribers[f.SessionID()]...)
	r.subMu.Unlock()
	for _, c := range chans {
		r.fanoutSend(c, f)
	}
}

// fanoutWarnAfter is the soft deadline before a slow subscriber
// trips a warn log. Tuned for the worst-case TUI render: a turn
// that emits a few hundred deltas should comfortably fit in the
// per-subscriber buffer; if a single delta waits this long, the
// adapter is stuck.
const fanoutWarnAfter = 30 * time.Second

// fanoutDrainGrace is how long a deregistered subscriber channel is drained
// (idle, self-resetting) before the drain goroutine exits — long enough to
// service any in-flight blocking fanoutSend that captured it before
// deregistration, bounded so a disconnect doesn't leak a goroutine for the
// process lifetime.
const fanoutDrainGrace = 10 * time.Second

// fanoutSend pushes one Frame onto one subscriber channel. Blocks
// until the subscriber drains so deltas accrue rather than getting
// silently dropped at the fanout. Three escape hatches keep the
// loop honest:
//
//   - ctx cancellation → bail out (runtime teardown).
//   - subscriber closed concurrently (Shutdown raced our send) →
//     the recover deferral absorbs the panic and returns.
//   - 30s soft warn → emits one log entry naming the session +
//     frame kind so a stuck subscriber is observable in real time
//     rather than discovered post-hoc; the send keeps waiting.
func (r *Runtime) fanoutSend(c chan protocol.Frame, f protocol.Frame) {
	// Recover catches send-on-closed-channel during Shutdown — the
	// channel is closed under subMu but fanoutSend reads its slice
	// snapshot lock-free, so a races against the close is possible
	// during graceful teardown. Debug log so a real panic from
	// anything else is visible rather than silently swallowed.
	defer func() {
		if rec := recover(); rec != nil && r.logger != nil {
			r.logger.Debug("runtime: fanout recovered from panic",
				"session", f.SessionID(),
				"frame_kind", string(f.Kind()),
				"panic", rec)
		}
	}()
	ctx := r.ctx
	if ctx == nil {
		// Defensive — NewRuntime sets r.ctx to Background; this
		// guard handles a hypothetical caller constructing the
		// Runtime via struct literal (skips NewRuntime).
		ctx = context.Background()
	}
	// Fast path: avoid timer allocation when the buffer has room.
	select {
	case c <- f:
		return
	default:
	}
	timer := time.NewTimer(fanoutWarnAfter)
	defer timer.Stop()
	select {
	case c <- f:
		return
	case <-ctx.Done():
		return
	case <-timer.C:
		if r.logger != nil {
			r.logger.Warn("runtime: subscriber slow drain",
				"session", f.SessionID(),
				"frame_kind", string(f.Kind()),
				"waited", fanoutWarnAfter,
				"buffer_cap", cap(c))
		}
	}
	select {
	case c <- f:
	case <-ctx.Done():
	}
}

// startSessionPump bridges a Session.Outbox to the runtime's
// subscriber list. One goroutine per live session; exits when the
// session goroutine closes its Outbox.
func (r *Runtime) startSessionPump(s *session.Session) {
	go func() {
		for f := range s.Outbox() {
			r.fanout(f)
		}
	}()
}

// adapterHost is the per-Run AdapterHost view passed to each Adapter.
type adapterHost struct {
	rt  *Runtime
	ctx context.Context
}

func (h *adapterHost) OpenSession(ctx context.Context, req session.OpenRequest) (*session.Session, time.Time, error) {
	s, openedAt, err := h.rt.manager.Open(ctx, req)
	if err != nil {
		return nil, time.Time{}, err
	}
	h.rt.startSessionPump(s)
	return s, openedAt, nil
}

func (h *adapterHost) ResumeSession(ctx context.Context, id string) (*session.Session, error) {
	s, err := h.rt.manager.Resume(ctx, id)
	if err != nil {
		return nil, err
	}
	h.rt.startSessionPump(s)
	return s, nil
}

func (h *adapterHost) Submit(ctx context.Context, f protocol.Frame) error {
	if f == nil {
		return fmt.Errorf("runtime: nil frame")
	}
	s, ok := h.rt.manager.Get(f.SessionID())
	if !ok {
		// No live session means the manager doesn't know it. Either
		// it never existed (404 territory; the post handler resumes
		// before Submit so this shouldn't fire for unknown ids) or
		// it just transitioned out of live state (Close raced our
		// post). Both surface as ErrSessionClosed for the adapter
		// layer; the post handler routes that to 409.
		return session.ErrSessionClosed
	}
	if s.IsClosed() {
		return session.ErrSessionClosed
	}
	select {
	case s.Inbox() <- f:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *adapterHost) Subscribe(ctx context.Context, sessionID string) (<-chan protocol.Frame, error) {
	// 256 is dimensioned to absorb a chatty turn's worth of
	// streaming deltas (gpt-class models can emit ~100-200
	// per-BPE-token deltas before any consumer-side render hops
	// can drain). The fanout send is blocking so headroom only
	// controls how often backpressure ripples back to the model
	// goroutine; lossless delivery is the channel's job, not the
	// buffer's.
	c := make(chan protocol.Frame, 256)
	h.rt.subMu.Lock()
	h.rt.subscribers[sessionID] = append(h.rt.subscribers[sessionID], c)
	h.rt.subMu.Unlock()
	go func() {
		<-ctx.Done()
		h.rt.subMu.Lock()
		// Drop our channel from the subscriber list. The runtime keeps
		// ownership of the channel close (Runtime.Shutdown closes
		// everything in the map at process exit); the adapter must NOT
		// range over c expecting it to close on its own ctx — it should
		// select on its own ctx.Done() alongside the channel.
		subs := h.rt.subscribers[sessionID]
		out := subs[:0]
		for _, sub := range subs {
			if sub != c {
				out = append(out, sub)
			}
		}
		h.rt.subscribers[sessionID] = out
		h.rt.subMu.Unlock()

		// Drain the deregistered channel. A blocking fanoutSend that
		// captured c in its lock-free slice snapshot BEFORE this
		// deregistration would otherwise park forever with no reader
		// (fanoutSend's only escape is runtime teardown) — filling the
		// session's outbox and wedging its Run loop permanently. No NEW
		// fanout targets c now (removed under subMu), so once in-flight
		// sends clear, the drain goes idle and exits; teardown also ends
		// it. Frames drained here are lost for this already-gone client
		// (a reconnect gets a fresh subscription + replay). Fixes the
		// dead-SSE-reader session freeze.
		t := time.NewTimer(fanoutDrainGrace)
		defer t.Stop()
		for {
			select {
			case <-c:
				t.Reset(fanoutDrainGrace)
			case <-t.C:
				return
			case <-h.rt.ctx.Done():
				return
			}
		}
	}()
	return c, nil
}

func (h *adapterHost) CloseSession(ctx context.Context, id, reason string) (time.Time, error) {
	if err := h.rt.manager.Terminate(ctx, id, reason); err != nil {
		return time.Time{}, err
	}
	return time.Now().UTC(), nil
}

func (h *adapterHost) ListSessions(ctx context.Context, status string) ([]session.SessionSummary, error) {
	return h.rt.manager.ListSessions(ctx, status)
}

func (h *adapterHost) ListEvents(ctx context.Context, sessionID string, opts store.ListEventsOpts) ([]store.EventRow, error) {
	return h.rt.manager.ListEvents(ctx, sessionID, opts)
}

func (h *adapterHost) SessionStats(ctx context.Context, sessionID string) (int, error) {
	return h.rt.manager.SessionStats(ctx, sessionID)
}

func (h *adapterHost) Logger() *slog.Logger { return h.rt.logger }
