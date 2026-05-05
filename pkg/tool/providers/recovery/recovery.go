package recovery

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/hugr-lab/hugen/pkg/tool"
)

// DefaultBackoff is the retry schedule applied when no
// WithBackoff option is supplied. Tuned for occasional MCP
// disconnects: a short first retry, then progressively longer
// gaps so a stuck upstream doesn't burn CPU.
var DefaultBackoff = []time.Duration{
	5 * time.Second,
	10 * time.Second,
	30 * time.Second,
}

// Provider decorates an inner tool.ToolProvider with
// retry-on-failure behaviour. List and Call attempt the
// underlying call once; on error, if the inner provider is
// tool.Recoverable, the wrapper walks the backoff schedule
// calling TryReconnect between attempts. The first successful
// retry returns; exhaustion returns the last error.
//
// All other ToolProvider methods (Name, Lifetime, Subscribe,
// Close) pass straight through to the inner.
type Provider struct {
	inner   tool.ToolProvider
	backoff []time.Duration
	log     *slog.Logger
}

// Option configures a freshly-built Provider.
type Option func(*Provider)

// WithBackoff overrides the default retry schedule. An empty slice
// disables retries on this wrapper.
func WithBackoff(steps ...time.Duration) Option {
	return func(p *Provider) {
		p.backoff = append([]time.Duration(nil), steps...)
	}
}

// WithLogger attaches a structured logger; nil falls back to a
// discard handler (default).
func WithLogger(log *slog.Logger) Option {
	return func(p *Provider) {
		if log != nil {
			p.log = log
		}
	}
}

// Wrap decorates inner with retry-on-failure behaviour. The
// resulting Provider passes through every ToolProvider method;
// retries fire only when the inner implements tool.Recoverable.
// Wrapping a nil inner panics.
func Wrap(inner tool.ToolProvider, opts ...Option) *Provider {
	if inner == nil {
		panic("recovery: nil inner provider")
	}
	p := &Provider{
		inner:   inner,
		backoff: append([]time.Duration(nil), DefaultBackoff...),
		log:     slog.New(slog.DiscardHandler),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Name forwards to the inner provider.
func (p *Provider) Name() string { return p.inner.Name() }

// Lifetime forwards to the inner provider.
func (p *Provider) Lifetime() tool.Lifetime { return p.inner.Lifetime() }

// Subscribe forwards to the inner provider — recovery does not
// synthesize events of its own.
func (p *Provider) Subscribe(ctx context.Context) (<-chan tool.ProviderEvent, error) {
	return p.inner.Subscribe(ctx)
}

// Close forwards to the inner provider. Idempotent if the inner
// is.
func (p *Provider) Close() error { return p.inner.Close() }

// List wraps inner.List with retry-on-failure semantics.
func (p *Provider) List(ctx context.Context) ([]tool.Tool, error) {
	return tryWithRecovery(ctx, p, func() ([]tool.Tool, error) {
		return p.inner.List(ctx)
	})
}

// Call wraps inner.Call with retry-on-failure semantics.
func (p *Provider) Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	return tryWithRecovery(ctx, p, func() (json.RawMessage, error) {
		return p.inner.Call(ctx, name, args)
	})
}

// Inner exposes the wrapped ToolProvider — convenient for tests
// and integrations that need to introspect past the decorator.
func (p *Provider) Inner() tool.ToolProvider { return p.inner }

// tryWithRecovery: try inner once; on error, if inner is
// Recoverable, walk the backoff calling TryReconnect + retry.
// Returns the last error after exhaustion.
func tryWithRecovery[T any](ctx context.Context, p *Provider, fn func() (T, error)) (T, error) {
	res, err := fn()
	if err == nil {
		return res, nil
	}
	r, ok := p.inner.(tool.Recoverable)
	if !ok {
		return res, err
	}
	for _, d := range p.backoff {
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(d):
		}
		if rerr := r.TryReconnect(ctx); rerr != nil {
			p.log.Warn("recovery: reconnect failed",
				"provider", p.inner.Name(),
				"delay", d,
				"err", rerr)
			continue
		}
		if res, err = fn(); err == nil {
			return res, nil
		}
		// Call still failing — could be permanent (e.g. auth denied).
		// Loop will try once more, then exhaust.
	}
	return res, err
}

// ensure Provider satisfies tool.ToolProvider.
var _ tool.ToolProvider = (*Provider)(nil)
