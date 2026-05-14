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
}

// NewRuntime constructs the supervisor. Adapters are started by Start.
func NewRuntime(manager *Manager, adapters []Adapter, logger *slog.Logger) *Runtime {
	if logger == nil {
		logger = slog.Default()
	}
	return &Runtime{
		manager:     manager,
		adapters:    adapters,
		logger:      logger,
		subscribers: make(map[string][]chan protocol.Frame),
	}
}

// Manager exposes the underlying Manager (used by main.go for
// boot-time resume).
func (r *Runtime) Manager() *Manager { return r.manager }

// Start runs every adapter under one errgroup and blocks until ctx
// is done or one of the adapters errors.
func (r *Runtime) Start(ctx context.Context) error {
	g, gctx := errgroup.WithContext(ctx)
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
// subMu; the actual sends happen unlocked so a slow subscriber
// never blocks the rest. Shutdown closes channels concurrently;
// the per-channel send is wrapped in a recover so the rare
// send-to-closed-channel race during teardown surfaces as a
// silent drop rather than a process panic.
func (r *Runtime) fanout(f protocol.Frame) {
	r.subMu.Lock()
	chans := append([]chan protocol.Frame(nil), r.subscribers[f.SessionID()]...)
	r.subMu.Unlock()
	for _, c := range chans {
		safeFanoutSend(c, f)
	}
}

// safeFanoutSend tries a non-blocking send and absorbs the panic
// from a concurrent close (Shutdown closed the channel between
// the snapshot copy and our send). Slow subscribers (full buffer)
// drop via the default branch.
func safeFanoutSend(c chan protocol.Frame, f protocol.Frame) {
	defer func() { _ = recover() }()
	select {
	case c <- f:
	default:
		// Slow subscriber — drop. Adapters that need lossless
		// streams must size their buffer accordingly.
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
	c := make(chan protocol.Frame, 64)
	h.rt.subMu.Lock()
	h.rt.subscribers[sessionID] = append(h.rt.subscribers[sessionID], c)
	h.rt.subMu.Unlock()
	go func() {
		<-ctx.Done()
		h.rt.subMu.Lock()
		defer h.rt.subMu.Unlock()
		// Drop our channel from the subscriber list. The runtime
		// keeps ownership of the channel close (Runtime.Shutdown
		// closes everything in the map at process exit); the
		// adapter must NOT range over c expecting it to close on
		// its own ctx — it should select on its own ctx.Done()
		// alongside the channel.
		subs := h.rt.subscribers[sessionID]
		out := subs[:0]
		for _, sub := range subs {
			if sub != c {
				out = append(out, sub)
			}
		}
		h.rt.subscribers[sessionID] = out
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
