package recovery

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/tool"
)

// flakyProvider implements tool.ToolProvider + tool.Recoverable
// with configurable failure / reconnect dynamics for the retry
// tests below.
type flakyProvider struct {
	name string

	listCalls    atomic.Int64
	callCalls    atomic.Int64
	reconnects   atomic.Int64
	failBeforeOk atomic.Int64 // n: List/Call returns err the first n times
	reconnectErr atomic.Bool  // when true, TryReconnect returns an error
}

func (f *flakyProvider) Name() string                                              { return f.name }
func (f *flakyProvider) Lifetime() tool.Lifetime                                   { return tool.LifetimePerAgent }
func (f *flakyProvider) Subscribe(context.Context) (<-chan tool.ProviderEvent, error) { return nil, nil }
func (f *flakyProvider) Close() error                                              { return nil }

func (f *flakyProvider) List(context.Context) ([]tool.Tool, error) {
	f.listCalls.Add(1)
	if f.failBeforeOk.Load() > 0 {
		f.failBeforeOk.Add(-1)
		return nil, errors.New("transient")
	}
	return []tool.Tool{{Name: f.name + ":ok"}}, nil
}

func (f *flakyProvider) Call(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	f.callCalls.Add(1)
	if f.failBeforeOk.Load() > 0 {
		f.failBeforeOk.Add(-1)
		return nil, errors.New("transient")
	}
	return json.RawMessage(`"ok"`), nil
}

func (f *flakyProvider) TryReconnect(context.Context) error {
	f.reconnects.Add(1)
	if f.reconnectErr.Load() {
		return errors.New("reconnect failed")
	}
	return nil
}

// nonRecoverableProvider is a plain ToolProvider that does NOT
// implement tool.Recoverable — the wrapper must NOT retry.
type nonRecoverableProvider struct {
	calls atomic.Int64
}

func (n *nonRecoverableProvider) Name() string                              { return "no-recover" }
func (n *nonRecoverableProvider) Lifetime() tool.Lifetime                   { return tool.LifetimePerAgent }
func (n *nonRecoverableProvider) Subscribe(context.Context) (<-chan tool.ProviderEvent, error) {
	return nil, nil
}
func (n *nonRecoverableProvider) Close() error { return nil }
func (n *nonRecoverableProvider) List(context.Context) ([]tool.Tool, error) {
	n.calls.Add(1)
	return nil, errors.New("permanent")
}
func (n *nonRecoverableProvider) Call(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	n.calls.Add(1)
	return nil, errors.New("permanent")
}

func TestWrap_NilInnerPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("Wrap(nil) did not panic")
		}
	}()
	_ = Wrap(nil)
}

func TestProvider_PassesThroughOnSuccess(t *testing.T) {
	inner := &flakyProvider{name: "ok"}
	p := Wrap(inner, WithBackoff(time.Millisecond))
	tools, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "ok:ok" {
		t.Errorf("List = %v", tools)
	}
	if inner.reconnects.Load() != 0 {
		t.Errorf("reconnect fired on success path")
	}
}

func TestProvider_RetriesAndRecovers(t *testing.T) {
	inner := &flakyProvider{name: "x"}
	inner.failBeforeOk.Store(2) // first two attempts error, third succeeds
	p := Wrap(inner, WithBackoff(time.Millisecond, time.Millisecond, time.Millisecond))

	out, err := p.Call(context.Background(), "ping", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if string(out) != `"ok"` {
		t.Errorf("Call result = %s", out)
	}
	if got := inner.callCalls.Load(); got != 3 {
		t.Errorf("call count = %d, want 3 (initial + 2 retries)", got)
	}
	if got := inner.reconnects.Load(); got != 2 {
		t.Errorf("reconnect count = %d, want 2", got)
	}
}

func TestProvider_NonRecoverableSurfacesErrorImmediately(t *testing.T) {
	inner := &nonRecoverableProvider{}
	p := Wrap(inner, WithBackoff(time.Millisecond))
	if _, err := p.List(context.Background()); err == nil {
		t.Fatal("expected error from non-recoverable inner")
	}
	if got := inner.calls.Load(); got != 1 {
		t.Errorf("non-recoverable retried — call count = %d, want 1", got)
	}
}

func TestProvider_BackoffExhaustionReturnsLastError(t *testing.T) {
	inner := &flakyProvider{name: "perm"}
	inner.failBeforeOk.Store(99) // never succeeds within backoff
	p := Wrap(inner, WithBackoff(time.Millisecond, time.Millisecond))
	if _, err := p.Call(context.Background(), "x", nil); err == nil {
		t.Fatal("expected exhaustion error")
	}
	// initial + 2 retries = 3 total Call attempts.
	if got := inner.callCalls.Load(); got != 3 {
		t.Errorf("call count = %d, want 3", got)
	}
}

func TestProvider_ReconnectErrorSkipsRetry(t *testing.T) {
	inner := &flakyProvider{name: "x"}
	inner.failBeforeOk.Store(99)
	inner.reconnectErr.Store(true)
	p := Wrap(inner, WithBackoff(time.Millisecond, time.Millisecond))
	if _, err := p.Call(context.Background(), "x", nil); err == nil {
		t.Fatal("expected exhaustion error")
	}
	// Initial Call only — reconnect failures skip the retry attempt.
	if got := inner.callCalls.Load(); got != 1 {
		t.Errorf("call count = %d, want 1 (no retries when reconnect fails)", got)
	}
	if got := inner.reconnects.Load(); got != 2 {
		t.Errorf("reconnect attempts = %d, want 2 (one per backoff step)", got)
	}
}

func TestProvider_ContextCancelStopsRetries(t *testing.T) {
	inner := &flakyProvider{name: "x"}
	inner.failBeforeOk.Store(99)
	p := Wrap(inner, WithBackoff(50*time.Millisecond, 50*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Call(ctx, "x", nil); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestProvider_PassthroughMethods(t *testing.T) {
	inner := &flakyProvider{name: "abc"}
	p := Wrap(inner)
	if p.Name() != "abc" {
		t.Errorf("Name = %q", p.Name())
	}
	if p.Lifetime() != tool.LifetimePerAgent {
		t.Errorf("Lifetime = %v", p.Lifetime())
	}
	if _, err := p.Subscribe(context.Background()); err != nil {
		t.Errorf("Subscribe: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if p.Inner() != inner {
		t.Errorf("Inner did not return wrapped provider")
	}
}
