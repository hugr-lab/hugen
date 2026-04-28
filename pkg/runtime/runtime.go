// Package runtime is the core of the hugen agent: it owns the
// supervisor goroutine, the SessionManager, and brokers Frame
// traffic between adapters and Sessions.
//
// Phase 1 ships a single Adapter (console) and a single Agent.
// Sub-agents, peer groups, and remote adapters are later phases.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/hugr-lab/hugen/pkg/protocol"
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
	OpenSession(ctx context.Context, req OpenRequest) (*Session, error)
	ResumeSession(ctx context.Context, id string) (*Session, error)
	Submit(ctx context.Context, frame protocol.Frame) error
	Subscribe(ctx context.Context, sessionID string) (<-chan protocol.Frame, error)
	Logger() *slog.Logger
}

// Runtime is the supervisor.
type Runtime struct {
	manager  *SessionManager
	adapters []Adapter
	logger   *slog.Logger

	subMu       sync.Mutex
	subscribers map[string][]chan protocol.Frame
}

// NewRuntime constructs the supervisor. Adapters are started by Start.
func NewRuntime(manager *SessionManager, adapters []Adapter, logger *slog.Logger) *Runtime {
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

// Manager exposes the underlying SessionManager (used by main.go for
// boot-time resume).
func (r *Runtime) Manager() *SessionManager { return r.manager }

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
	r.manager.ShutdownAll(ctx)
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
func (r *Runtime) fanout(f protocol.Frame) {
	r.subMu.Lock()
	chans := append([]chan protocol.Frame(nil), r.subscribers[f.SessionID()]...)
	r.subMu.Unlock()
	for _, c := range chans {
		select {
		case c <- f:
		default:
			// Slow subscriber — drop. Adapters that need lossless
			// streams must size their buffer accordingly.
		}
	}
}

// startSessionPump bridges a Session.Outbox to the runtime's
// subscriber list. One goroutine per live session; exits when the
// session goroutine closes its Outbox.
func (r *Runtime) startSessionPump(s *Session) {
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

func (h *adapterHost) OpenSession(ctx context.Context, req OpenRequest) (*Session, error) {
	s, err := h.rt.manager.Open(ctx, req)
	if err != nil {
		return nil, err
	}
	h.rt.startSessionPump(s)
	return s, nil
}

func (h *adapterHost) ResumeSession(ctx context.Context, id string) (*Session, error) {
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
		return fmt.Errorf("runtime: no live session %q", f.SessionID())
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
		// Drop our channel from the subscriber list.
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

func (h *adapterHost) Logger() *slog.Logger { return h.rt.logger }
