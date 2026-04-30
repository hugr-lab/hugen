package tool

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
)

// fakeProvider is a configurable ToolProvider for tests.
type fakeProvider struct {
	name     string
	tools    []Tool
	calls    atomic.Int64
	closed   atomic.Bool
	callFunc func(name string, args json.RawMessage) (json.RawMessage, error)
}

func (f *fakeProvider) Name() string                     { return f.name }
func (f *fakeProvider) Lifetime() Lifetime               { return LifetimePerAgent }
func (f *fakeProvider) List(ctx context.Context) ([]Tool, error) {
	out := make([]Tool, len(f.tools))
	copy(out, f.tools)
	return out, nil
}
func (f *fakeProvider) Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	f.calls.Add(1)
	if f.callFunc != nil {
		return f.callFunc(name, args)
	}
	return json.RawMessage(`{"ok":true}`), nil
}
func (f *fakeProvider) Subscribe(ctx context.Context) (<-chan ProviderEvent, error) {
	return nil, nil
}
func (f *fakeProvider) Close() error {
	f.closed.Store(true)
	return nil
}

// fakePerms is a perm.Service stub. By default returns a zero
// Permission (allow); rules map lets tests inject denies/data.
type fakePerms struct {
	rules map[string]perm.Permission
}

func (f *fakePerms) Resolve(ctx context.Context, object, field string) (perm.Permission, error) {
	if p, ok := f.rules[object+":"+field]; ok {
		return p, nil
	}
	if p, ok := f.rules[object+":*"]; ok {
		return p, nil
	}
	return perm.Permission{}, nil
}
func (f *fakePerms) Refresh(ctx context.Context) error                            { return nil }
func (f *fakePerms) Subscribe(ctx context.Context) (<-chan perm.RefreshEvent, error) { return nil, nil }

func TestToolManager_AddRemoveProvider(t *testing.T) {
	m := NewToolManager(&fakePerms{}, nil, Options{DrainTimeout: 10 * time.Millisecond})

	p := &fakeProvider{name: "bash-mcp", tools: []Tool{
		{Name: "bash-mcp:bash.read_file", Provider: "bash-mcp", PermissionObject: "hugen:tool:bash-mcp"},
	}}
	if err := m.AddProvider(p); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	if err := m.AddProvider(p); err == nil {
		t.Errorf("AddProvider(dup) returned nil error")
	}
	if got := m.Providers(); len(got) != 1 || got[0] != "bash-mcp" {
		t.Errorf("Providers = %v", got)
	}
	if err := m.RemoveProvider(context.Background(), "bash-mcp"); err != nil {
		t.Fatalf("RemoveProvider: %v", err)
	}
	if !p.closed.Load() {
		t.Errorf("RemoveProvider did not call Close")
	}
	if err := m.RemoveProvider(context.Background(), "bash-mcp"); !errors.Is(err, ErrUnknownProvider) {
		t.Errorf("RemoveProvider(missing) = %v, want ErrUnknownProvider", err)
	}
}

func TestToolManager_Snapshot_NoSkillsAllProvidersExposed(t *testing.T) {
	m := NewToolManager(&fakePerms{}, nil, Options{}) // skills=nil → no filter
	p := &fakeProvider{name: "bash-mcp", tools: []Tool{
		{Name: "bash-mcp:bash.read_file", Provider: "bash-mcp"},
		{Name: "bash-mcp:bash.write_file", Provider: "bash-mcp"},
	}}
	if err := m.AddProvider(p); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	snap, err := m.Snapshot(context.Background(), "s1")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap.Tools) != 2 {
		t.Errorf("snap.Tools len = %d, want 2", len(snap.Tools))
	}
}

func TestToolManager_Snapshot_RebuildOnGenerationMove(t *testing.T) {
	m := NewToolManager(&fakePerms{}, nil, Options{})
	p1 := &fakeProvider{name: "p1", tools: []Tool{{Name: "p1:a", Provider: "p1"}}}
	if err := m.AddProvider(p1); err != nil {
		t.Fatal(err)
	}

	first, _ := m.Snapshot(context.Background(), "s")
	if len(first.Tools) != 1 {
		t.Fatalf("first snapshot len = %d, want 1", len(first.Tools))
	}

	p2 := &fakeProvider{name: "p2", tools: []Tool{{Name: "p2:b", Provider: "p2"}}}
	if err := m.AddProvider(p2); err != nil {
		t.Fatal(err)
	}

	second, _ := m.Snapshot(context.Background(), "s")
	if len(second.Tools) != 2 {
		t.Errorf("second snapshot len = %d, want 2 (rebuild after AddProvider)", len(second.Tools))
	}
	if second.Generations.Tool == first.Generations.Tool {
		t.Errorf("Generations.Tool did not move: %d", second.Generations.Tool)
	}
}

func TestToolManager_Snapshot_StableWithinGeneration(t *testing.T) {
	m := NewToolManager(&fakePerms{}, nil, Options{})
	if err := m.AddProvider(&fakeProvider{name: "p", tools: []Tool{{Name: "p:t", Provider: "p"}}}); err != nil {
		t.Fatal(err)
	}
	a, _ := m.Snapshot(context.Background(), "s")
	b, _ := m.Snapshot(context.Background(), "s")
	if a.Generations != b.Generations {
		t.Errorf("Generations moved without a state change: %+v -> %+v", a.Generations, b.Generations)
	}
}

func TestToolManager_Resolve_Denied(t *testing.T) {
	perms := &fakePerms{rules: map[string]perm.Permission{
		"hugen:tool:bash-mcp:bash.write_file": {Disabled: true, FromConfig: true},
	}}
	m := NewToolManager(perms, nil, Options{})
	tool := Tool{Name: "bash-mcp:bash.write_file", Provider: "bash-mcp", PermissionObject: "hugen:tool:bash-mcp"}
	_, _, err := m.Resolve(context.Background(), tool, json.RawMessage(`{}`))
	if !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("err = %v, want ErrPermissionDenied", err)
	}
}

func TestToolManager_Resolve_DataMergedRuleWins(t *testing.T) {
	perms := &fakePerms{rules: map[string]perm.Permission{
		"hugen:tool:bash-mcp:bash.run": {
			FromConfig: true,
			Data:       json.RawMessage(`{"workspace":"/var/agents/x"}`),
		},
	}}
	m := NewToolManager(perms, nil, Options{})
	tool := Tool{Name: "bash-mcp:bash.run", Provider: "bash-mcp", PermissionObject: "hugen:tool:bash-mcp"}
	args := json.RawMessage(`{"cmd":"ls","workspace":"/tmp/llm-supplied"}`)
	_, eff, err := m.Resolve(context.Background(), tool, args)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(eff, &got); err != nil {
		t.Fatal(err)
	}
	if got["workspace"] != "/var/agents/x" {
		t.Errorf("workspace = %v, want /var/agents/x (rule wins)", got["workspace"])
	}
	if got["cmd"] != "ls" {
		t.Errorf("cmd = %v, want preserved", got["cmd"])
	}
}

func TestToolManager_Dispatch_RoutesToProvider(t *testing.T) {
	called := atomic.Int64{}
	p := &fakeProvider{
		name: "bash-mcp",
		callFunc: func(name string, args json.RawMessage) (json.RawMessage, error) {
			called.Add(1)
			if name != "bash.run" {
				return nil, errors.New("unexpected name: " + name)
			}
			return json.RawMessage(`{"out":"ok"}`), nil
		},
	}
	m := NewToolManager(&fakePerms{}, nil, Options{})
	if err := m.AddProvider(p); err != nil {
		t.Fatal(err)
	}
	tool := Tool{Name: "bash-mcp:bash.run", Provider: "bash-mcp", PermissionObject: "hugen:tool:bash-mcp"}
	out, err := m.Dispatch(context.Background(), tool, json.RawMessage(`{"cmd":"ls"}`))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if called.Load() != 1 {
		t.Errorf("provider Call count = %d, want 1", called.Load())
	}
	if string(out) != `{"out":"ok"}` {
		t.Errorf("Dispatch result = %s", out)
	}
}

func TestToolManager_Dispatch_UnknownProvider(t *testing.T) {
	m := NewToolManager(&fakePerms{}, nil, Options{})
	tool := Tool{Name: "ghost:tool", Provider: "ghost"}
	_, err := m.Dispatch(context.Background(), tool, nil)
	if !errors.Is(err, ErrUnknownProvider) {
		t.Errorf("err = %v, want ErrUnknownProvider", err)
	}
}

func TestToolManager_BumpPolicyGen_InvalidatesCache(t *testing.T) {
	m := NewToolManager(&fakePerms{}, nil, Options{})
	if err := m.AddProvider(&fakeProvider{name: "p", tools: []Tool{{Name: "p:t", Provider: "p"}}}); err != nil {
		t.Fatal(err)
	}
	a, _ := m.Snapshot(context.Background(), "s")
	m.BumpPolicyGen()
	b, _ := m.Snapshot(context.Background(), "s")
	if b.Generations.Policy == a.Generations.Policy {
		t.Errorf("Policy gen did not move: %d", b.Generations.Policy)
	}
}

func TestToolManager_SessionProvider_VisibleOnlyToOwningSession(t *testing.T) {
	m := NewToolManager(&fakePerms{}, nil, Options{DrainTimeout: 10 * time.Millisecond})
	global := &fakeProvider{name: "system", tools: []Tool{{Name: "system:notepad", Provider: "system"}}}
	if err := m.AddProvider(global); err != nil {
		t.Fatal(err)
	}
	scoped := &fakeProvider{name: "bash-mcp", tools: []Tool{{Name: "bash-mcp:bash.run", Provider: "bash-mcp"}}}
	if err := m.AddSessionProvider("s1", scoped); err != nil {
		t.Fatal(err)
	}

	s1, _ := m.Snapshot(context.Background(), "s1")
	if len(s1.Tools) != 2 {
		t.Errorf("s1 tools = %d, want 2 (global + scoped)", len(s1.Tools))
	}
	s2, _ := m.Snapshot(context.Background(), "s2")
	if len(s2.Tools) != 1 {
		t.Errorf("s2 tools = %d, want 1 (global only)", len(s2.Tools))
	}
}

func TestToolManager_SessionProvider_ShadowsGlobalOnDispatch(t *testing.T) {
	m := NewToolManager(&fakePerms{}, nil, Options{DrainTimeout: 10 * time.Millisecond})
	global := &fakeProvider{name: "bash-mcp", callFunc: func(name string, args json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"from":"global"}`), nil
	}}
	scoped := &fakeProvider{name: "bash-mcp", callFunc: func(name string, args json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"from":"scoped"}`), nil
	}}
	if err := m.AddProvider(global); err != nil {
		t.Fatal(err)
	}
	if err := m.AddSessionProvider("s1", scoped); err != nil {
		t.Fatal(err)
	}
	tool := Tool{Name: "bash-mcp:bash.run", Provider: "bash-mcp"}
	ctx := perm.WithSession(context.Background(), perm.SessionContext{SessionID: "s1"})
	out, err := m.Dispatch(ctx, tool, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if string(out) != `{"from":"scoped"}` {
		t.Errorf("got %s, want scoped result", out)
	}

	ctx2 := perm.WithSession(context.Background(), perm.SessionContext{SessionID: "s2"})
	out2, _ := m.Dispatch(ctx2, tool, json.RawMessage(`{}`))
	if string(out2) != `{"from":"global"}` {
		t.Errorf("got %s, want global result for s2", out2)
	}
}

func TestToolManager_CloseSession_TearsDownProviders(t *testing.T) {
	m := NewToolManager(&fakePerms{}, nil, Options{DrainTimeout: 10 * time.Millisecond})
	p := &fakeProvider{name: "bash-mcp"}
	if err := m.AddSessionProvider("s1", p); err != nil {
		t.Fatal(err)
	}
	if err := m.CloseSession(context.Background(), "s1"); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	if !p.closed.Load() {
		t.Errorf("provider not closed")
	}
	snap, _ := m.Snapshot(context.Background(), "s1")
	if len(snap.Tools) != 0 {
		t.Errorf("tools after close = %d, want 0", len(snap.Tools))
	}
}
