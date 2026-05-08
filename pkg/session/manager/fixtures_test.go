package manager

// Mirrored from pkg/session test fixtures — see
// pkg/session/test_session_fixture_test.go,
// pkg/session/session_test.go,
// pkg/session/session_tools_test.go.
//
// Duplication accepted to keep manager-side tests free of cross-package
// internals. These helpers are tiny and stable; if you need to evolve
// them, update both copies.

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/identity"
	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// scriptedModel emits a fixed sequence of chunks then ends.
type scriptedModel struct {
	chunks []model.Chunk
	mu     sync.Mutex
	calls  int
}

func (m *scriptedModel) Spec() model.ModelSpec {
	return model.ModelSpec{Provider: "fake", Name: "test"}
}

func (m *scriptedModel) Generate(_ context.Context, _ model.Request) (model.Stream, error) {
	m.mu.Lock()
	m.calls++
	chunks := append([]model.Chunk(nil), m.chunks...)
	m.mu.Unlock()
	ch := make(chan model.Chunk, len(chunks))
	for _, c := range chunks {
		ch <- c
	}
	close(ch)
	return &scriptedStream{ch: ch}, nil
}

type scriptedStream struct {
	ch   chan model.Chunk
	done bool
}

func (s *scriptedStream) Next(_ context.Context) (model.Chunk, bool, error) {
	c, ok := <-s.ch
	if !ok {
		return model.Chunk{}, false, nil
	}
	return c, true, nil
}

func (s *scriptedStream) Close() error { s.done = true; return nil }

// scriptedToolModel emits tool calls on its first N invocations
// and final content on the last invocation. Used by ceiling/cascade
// scenarios driven through the public Manager surface.
type scriptedToolModel struct {
	turns [][]model.Chunk
	calls int
	mu    sync.Mutex
}

func (m *scriptedToolModel) Spec() model.ModelSpec {
	return model.ModelSpec{Provider: "fake", Name: "tooly"}
}

func (m *scriptedToolModel) Generate(_ context.Context, _ model.Request) (model.Stream, error) {
	m.mu.Lock()
	idx := m.calls
	m.calls++
	m.mu.Unlock()
	if idx >= len(m.turns) {
		return nil, errors.New("scriptedToolModel: out of scripted turns")
	}
	chunks := append([]model.Chunk(nil), m.turns[idx]...)
	ch := make(chan model.Chunk, len(chunks))
	for _, c := range chunks {
		ch <- c
	}
	close(ch)
	return &scriptedStream{ch: ch}, nil
}

// fakeIdentity satisfies pkg/identity.Source for tests.
type fakeIdentity struct{ id string }

func (f *fakeIdentity) Agent(_ context.Context) (identity.Agent, error) {
	return identity.Agent{ID: f.id, Name: "hugen", Type: "test"}, nil
}
func (f *fakeIdentity) WhoAmI(_ context.Context) (identity.WhoAmI, error) {
	return identity.WhoAmI{UserID: f.id, UserName: "hugen", Role: "agent"}, nil
}
func (f *fakeIdentity) Permission(_ context.Context, _, _ string) (identity.Permission, error) {
	return identity.Permission{Enabled: true}, nil
}

func newRouterWithModel(t *testing.T, m model.Model) *model.ModelRouter {
	t.Helper()
	defaults := map[model.Intent]model.ModelSpec{
		model.IntentDefault: m.Spec(),
		model.IntentCheap:   m.Spec(),
	}
	models := map[model.ModelSpec]model.Model{m.Spec(): m}
	r, err := model.NewModelRouter(defaults, models)
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	return r
}

// permsAllow always returns Permission{} (default-allow).
type permsAllow struct{}

func (permsAllow) Resolve(_ context.Context, _, _ string) (perm.Permission, error) {
	return perm.Permission{}, nil
}
func (permsAllow) Refresh(_ context.Context) error                               { return nil }
func (permsAllow) Subscribe(_ context.Context) (<-chan perm.RefreshEvent, error) { return nil, nil }

// drainOutboxOnce reads up to one frame off the outbox or returns
// after a short timeout. Tests use it to swallow lifecycle frames
// (SessionOpened, etc.) before driving the actual scenario.
func drainOutboxOnce(out <-chan protocol.Frame) {
	select {
	case <-out:
	case <-time.After(200 * time.Millisecond):
	}
}

// kindsOnly is a debug helper for failing assertions — extracts the
// sequence of event kinds for a more readable error message.
func kindsOnly(rows []session.EventRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.EventType)
	}
	return out
}

// stubProvider is a one-tool ToolProvider used in tool-dispatch tests.
type stubProvider struct {
	tools  []tool.Tool
	result string
}

func (p *stubProvider) Name() string                                { return "fake" }
func (p *stubProvider) Lifetime() tool.Lifetime                     { return tool.LifetimePerAgent }
func (p *stubProvider) List(_ context.Context) ([]tool.Tool, error) { return p.tools, nil }
func (p *stubProvider) Call(_ context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(p.result), nil
}
func (p *stubProvider) Subscribe(_ context.Context) (<-chan tool.ProviderEvent, error) {
	return nil, nil
}
func (p *stubProvider) Close() error { return nil }

// collectFrames reads frames off sess.Outbox() until `until` returns
// true or `deadline` elapses. Used by ceiling tests to wait for a
// final agent message before reading the events log.
func collectFrames(t *testing.T, sess *session.Session, until func(seen []protocol.Frame) bool, deadline time.Duration) []protocol.Frame {
	t.Helper()
	var seen []protocol.Frame
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	for {
		select {
		case f, ok := <-sess.Outbox():
			if !ok {
				return seen
			}
			seen = append(seen, f)
			if until(seen) {
				return seen
			}
		case <-timer.C:
			t.Fatalf("timeout; seen=%v", kindNames(seen))
			return seen
		}
	}
}

func kindNames(fs []protocol.Frame) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = string(f.Kind())
	}
	return out
}
