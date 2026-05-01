package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// scriptedToolModel emits tool calls on its first N invocations
// and final content on the last invocation. Used to drive the
// Session's tool-dispatch + re-call loop.
type scriptedToolModel struct {
	turns [][]model.Chunk
	calls atomic.Int32
}

func (m *scriptedToolModel) Spec() model.ModelSpec {
	return model.ModelSpec{Provider: "fake", Name: "tooly"}
}

func (m *scriptedToolModel) Generate(_ context.Context, _ model.Request) (model.Stream, error) {
	idx := int(m.calls.Add(1)) - 1
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

// stubProvider is a one-tool ToolProvider used in tool-dispatch tests.
type stubProvider struct {
	tools  []tool.Tool
	result string
}

func (p *stubProvider) Name() string                            { return "fake" }
func (p *stubProvider) Lifetime() tool.Lifetime                 { return tool.LifetimePerAgent }
func (p *stubProvider) List(_ context.Context) ([]tool.Tool, error) { return p.tools, nil }
func (p *stubProvider) Call(_ context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(p.result), nil
}
func (p *stubProvider) Subscribe(_ context.Context) (<-chan tool.ProviderEvent, error) {
	return nil, nil
}
func (p *stubProvider) Close() error { return nil }

// permsAllow always returns Permission{} (default-allow).
type permsAllow struct{}

func (permsAllow) Resolve(_ context.Context, _, _ string) (perm.Permission, error) {
	return perm.Permission{}, nil
}
func (permsAllow) Refresh(_ context.Context) error                            { return nil }
func (permsAllow) Subscribe(_ context.Context) (<-chan perm.RefreshEvent, error) { return nil, nil }

// permsDeny denies a fixed (object, field).
type permsDeny struct {
	object, field string
}

func (d permsDeny) Resolve(_ context.Context, object, field string) (perm.Permission, error) {
	if object == d.object && (field == d.field || d.field == "*") {
		return perm.Permission{Disabled: true, FromConfig: true}, nil
	}
	return perm.Permission{}, nil
}
func (permsDeny) Refresh(_ context.Context) error                            { return nil }
func (permsDeny) Subscribe(_ context.Context) (<-chan perm.RefreshEvent, error) { return nil, nil }

func newToolSession(t *testing.T, mdl model.Model, perms perm.Service, providers ...tool.ToolProvider) (*Session, context.CancelFunc) {
	t.Helper()
	store := newFakeStore()
	_ = store.OpenSession(context.Background(), SessionRow{ID: "s1", AgentID: "a1", Status: StatusActive})

	tm := tool.NewToolManager(perms, nil, nil, nil, nil)
	for _, p := range providers {
		if err := tm.AddProvider(p); err != nil {
			t.Fatalf("AddProvider: %v", err)
		}
	}

	router := newRouterWithModel(t, mdl)
	agent, err := NewAgent("a1", "hugen", &fakeIdentity{id: "a1"}, "")
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	sess := NewSession("s1", agent, store, router, NewCommandRegistry(), protocol.NewCodec(), nil, WithTools(tm))
	sess.materialised.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = sess.Run(ctx) }()
	return sess, cancel
}

func collectFrames(t *testing.T, sess *Session, until func(seen []protocol.Frame) bool, deadline time.Duration) []protocol.Frame {
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

func TestSession_ToolDispatch_HappyPath(t *testing.T) {
	mdl := &scriptedToolModel{
		turns: [][]model.Chunk{
			{
				{ToolCall: &model.ChunkToolCall{ID: "tc1", Name: "fake:do", Args: map[string]any{"x": 1}}},
			},
			{
				{Content: ptr("done"), Final: true},
			},
		},
	}
	provider := &stubProvider{
		tools:  []tool.Tool{{Name: "fake:do", Provider: "fake", PermissionObject: "hugen:tool:fake"}},
		result: `{"echo":"ok"}`,
	}
	sess, cancel := newToolSession(t, mdl, permsAllow{}, provider)
	defer cancel()

	user := protocol.ParticipantInfo{ID: "u1", Kind: protocol.ParticipantUser}
	sess.Inbox() <- protocol.NewUserMessage("s1", user, "go")

	frames := collectFrames(t, sess, func(seen []protocol.Frame) bool {
		if am, ok := seen[len(seen)-1].(*protocol.AgentMessage); ok && am.Payload.Final {
			return true
		}
		return false
	}, 3*time.Second)

	kinds := kindNames(frames)
	wantContains := []string{
		string(protocol.KindUserMessage),
		string(protocol.KindToolCall),
		string(protocol.KindToolResult),
		string(protocol.KindAgentMessage),
	}
	for _, want := range wantContains {
		if !contains(kinds, want) {
			t.Errorf("missing %s in %v", want, kinds)
		}
	}
	if mdl.calls.Load() != 2 {
		t.Errorf("model.Generate calls = %d, want 2 (initial + post-tool)", mdl.calls.Load())
	}
}

func TestSession_ToolDispatch_PermissionDenied(t *testing.T) {
	mdl := &scriptedToolModel{
		turns: [][]model.Chunk{
			{
				{ToolCall: &model.ChunkToolCall{ID: "tc1", Name: "fake:do"}},
			},
			{
				{Content: ptr("acknowledged"), Final: true},
			},
		},
	}
	provider := &stubProvider{
		tools:  []tool.Tool{{Name: "fake:do", Provider: "fake", PermissionObject: "hugen:tool:fake"}},
		result: `{}`,
	}
	sess, cancel := newToolSession(t, mdl, permsDeny{object: "hugen:tool:fake", field: "*"}, provider)
	defer cancel()

	user := protocol.ParticipantInfo{ID: "u1", Kind: protocol.ParticipantUser}
	sess.Inbox() <- protocol.NewUserMessage("s1", user, "go")

	frames := collectFrames(t, sess, func(seen []protocol.Frame) bool {
		if am, ok := seen[len(seen)-1].(*protocol.AgentMessage); ok && am.Payload.Final {
			return true
		}
		return false
	}, 3*time.Second)

	// Look for the tool_result with IsError + tool_denied marker.
	var denied bool
	var marker bool
	for _, f := range frames {
		if tr, ok := f.(*protocol.ToolResult); ok && tr.Payload.IsError {
			body, _ := json.Marshal(tr.Payload.Result)
			if strings.Contains(string(body), protocol.ToolErrorPermissionDenied) {
				denied = true
			}
		}
		if mk, ok := f.(*protocol.SystemMarker); ok && mk.Payload.Subject == protocol.SubjectToolDenied {
			marker = true
		}
	}
	if !denied {
		t.Errorf("missing tool_result{permission_denied} in %v", kindNames(frames))
	}
	if !marker {
		t.Errorf("missing tool_denied marker in %v", kindNames(frames))
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
