package tool

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

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
	m := NewToolManager(&fakePerms{}, nil, nil)

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
	m := NewToolManager(&fakePerms{}, nil, nil) // skills=nil → no filter
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

// NOTE: The phase-3 wildcard-grant filtering test moved to
// pkg/session (snapshot_cache_test.go) when stage A step 7c+8
// extracted skill-bindings filtering out of ToolManager. Manager
// now returns the unfiltered union; pkg/session caches and
// filters per-session via the loaded skill bindings.

func TestToolManager_Snapshot_RebuildOnGenerationMove(t *testing.T) {
	m := NewToolManager(&fakePerms{}, nil, nil)
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
	m := NewToolManager(&fakePerms{}, nil, nil)
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
	m := NewToolManager(perms, nil, nil)
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
	m := NewToolManager(perms, nil, nil)
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
	m := NewToolManager(&fakePerms{}, nil, nil)
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
	m := NewToolManager(&fakePerms{}, nil, nil)
	tool := Tool{Name: "ghost:tool", Provider: "ghost"}
	_, err := m.Dispatch(context.Background(), tool, nil)
	if !errors.Is(err, ErrUnknownProvider) {
		t.Errorf("err = %v, want ErrUnknownProvider", err)
	}
}

func TestToolManager_BumpPolicyGen_InvalidatesCache(t *testing.T) {
	m := NewToolManager(&fakePerms{}, nil, nil)
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

// Phase 4.1a stage A step 9: per-session providers now live on
// a child Manager built via NewChild — the tests below pin the
// equivalent semantics through that surface (replacing the
// retired AddSessionProvider / CloseSession path).

func TestToolManager_Child_VisibleOnlyToOwningSession(t *testing.T) {
	root := NewToolManager(&fakePerms{}, nil, nil)
	global := &fakeProvider{name: "system", tools: []Tool{{Name: "system:notepad", Provider: "system"}}}
	if err := root.AddProvider(global); err != nil {
		t.Fatal(err)
	}
	s1Tools := root.NewChild()
	scoped := &fakeProvider{name: "bash-mcp", tools: []Tool{{Name: "bash-mcp:bash.run", Provider: "bash-mcp"}}}
	if err := s1Tools.AddProvider(scoped); err != nil {
		t.Fatal(err)
	}

	s1, _ := s1Tools.Snapshot(context.Background(), "s1")
	if len(s1.Tools) != 2 {
		t.Errorf("s1 tools = %d, want 2 (global walked from parent + scoped on child)", len(s1.Tools))
	}
	// Sibling session has its own (empty) child — root shows global only.
	s2, _ := root.Snapshot(context.Background(), "s2")
	if len(s2.Tools) != 1 {
		t.Errorf("s2 tools = %d, want 1 (global only)", len(s2.Tools))
	}
}

func TestToolManager_Child_ShadowsGlobalOnDispatch(t *testing.T) {
	root := NewToolManager(&fakePerms{}, nil, nil)
	global := &fakeProvider{name: "bash-mcp", callFunc: func(name string, args json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"from":"global"}`), nil
	}}
	scoped := &fakeProvider{name: "bash-mcp", callFunc: func(name string, args json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"from":"scoped"}`), nil
	}}
	if err := root.AddProvider(global); err != nil {
		t.Fatal(err)
	}
	s1Tools := root.NewChild()
	if err := s1Tools.AddProvider(scoped); err != nil {
		t.Fatal(err)
	}
	tool := Tool{Name: "bash-mcp:bash.run", Provider: "bash-mcp"}
	out, err := s1Tools.Dispatch(context.Background(), tool, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if string(out) != `{"from":"scoped"}` {
		t.Errorf("got %s, want scoped result", out)
	}

	// Sibling session uses its own child (no override) — sees the global.
	s2Tools := root.NewChild()
	out2, _ := s2Tools.Dispatch(context.Background(), tool, json.RawMessage(`{}`))
	if string(out2) != `{"from":"global"}` {
		t.Errorf("got %s, want global result for s2", out2)
	}
}

func TestToolManager_Child_CloseTearsDownProviders(t *testing.T) {
	root := NewToolManager(&fakePerms{}, nil, nil)
	child := root.NewChild()
	p := &fakeProvider{name: "bash-mcp"}
	if err := child.AddProvider(p); err != nil {
		t.Fatal(err)
	}
	if err := child.Close(); err != nil {
		t.Fatalf("child.Close: %v", err)
	}
	if !p.closed.Load() {
		t.Errorf("provider not closed")
	}
	snap, _ := child.Snapshot(context.Background(), "s1")
	if len(snap.Tools) != 0 {
		t.Errorf("tools after close = %d, want 0", len(snap.Tools))
	}
}

// fakePermsWithAgent is a fakePerms that also satisfies the
// AgentID accessor ToolManager.Resolve looks for to feed Tier 3.
type fakePermsWithAgent struct {
	fakePerms
	agentID string
}

func (f *fakePermsWithAgent) AgentID() string { return f.agentID }

func newPoliciesForTest(t *testing.T) (*Policies, *fakePolicyStore) {
	t.Helper()
	store := newFakePolicyStore()
	return NewPolicies(newFakePolicyQuerier(store)), store
}

func TestToolManager_Resolve_Tier3DenyBlocks(t *testing.T) {
	pol, _ := newPoliciesForTest(t)
	if _, err := pol.Save(context.Background(), PolicyInput{
		AgentID:  "ag01",
		ToolName: "bash-mcp:read_file",
		Decision: PolicyDeny,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	perms := &fakePermsWithAgent{agentID: "ag01"}
	m := NewToolManager(perms, nil, nil)
	m.SetPolicies(pol)

	tl := Tool{
		Name:             "bash-mcp:read_file",
		Provider:         "bash-mcp",
		PermissionObject: "hugen:tool:bash-mcp",
	}
	got, _, err := m.Resolve(context.Background(), tl, json.RawMessage(`{}`))
	if !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("err = %v, want ErrPermissionDenied", err)
	}
	if !got.FromUser {
		t.Errorf("FromUser = false, want true (Tier 3 decided)")
	}
}

func TestToolManager_Resolve_Tier3AllowMarksFromUser(t *testing.T) {
	pol, _ := newPoliciesForTest(t)
	if _, err := pol.Save(context.Background(), PolicyInput{
		AgentID:  "ag01",
		ToolName: "bash-mcp:read_file",
		Decision: PolicyAllow,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	perms := &fakePermsWithAgent{agentID: "ag01"}
	m := NewToolManager(perms, nil, nil)
	m.SetPolicies(pol)

	tl := Tool{
		Name:             "bash-mcp:read_file",
		Provider:         "bash-mcp",
		PermissionObject: "hugen:tool:bash-mcp",
	}
	got, _, err := m.Resolve(context.Background(), tl, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !got.FromUser {
		t.Errorf("FromUser = false, want true on allow")
	}
}

func TestToolManager_Resolve_Tier1FloorBeatsTier3Allow(t *testing.T) {
	pol, _ := newPoliciesForTest(t)
	if _, err := pol.Save(context.Background(), PolicyInput{
		AgentID:  "ag01",
		ToolName: "bash-mcp:write_file",
		Decision: PolicyAllow,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	perms := &fakePermsWithAgent{
		fakePerms: fakePerms{rules: map[string]perm.Permission{
			"hugen:tool:bash-mcp:*": {Disabled: true, FromConfig: true},
		}},
		agentID: "ag01",
	}
	m := NewToolManager(perms, nil, nil)
	m.SetPolicies(pol)

	tl := Tool{
		Name:             "bash-mcp:write_file",
		Provider:         "bash-mcp",
		PermissionObject: "hugen:tool:bash-mcp",
	}
	got, _, err := m.Resolve(context.Background(), tl, json.RawMessage(`{}`))
	if !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("err = %v, want ErrPermissionDenied", err)
	}
	if got.FromUser {
		t.Errorf("FromUser = true; floor must win without consulting Tier 3")
	}
	if !got.FromConfig {
		t.Errorf("FromConfig = false; floor not recorded")
	}
}

func TestToolManager_Resolve_NoPoliciesSkipsTier3(t *testing.T) {
	perms := &fakePermsWithAgent{agentID: "ag01"}
	m := NewToolManager(perms, nil, nil) // no SetPolicies
	tl := Tool{
		Name:             "bash-mcp:read_file",
		Provider:         "bash-mcp",
		PermissionObject: "hugen:tool:bash-mcp",
	}
	got, _, err := m.Resolve(context.Background(), tl, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.FromUser {
		t.Errorf("FromUser = true with no Policies")
	}
}

func TestToolManager_Resolve_AskFallsThrough(t *testing.T) {
	pol, _ := newPoliciesForTest(t)
	// no row → Decide returns Ask; Resolve should not mark FromUser.
	perms := &fakePermsWithAgent{agentID: "ag01"}
	m := NewToolManager(perms, nil, nil)
	m.SetPolicies(pol)
	tl := Tool{
		Name:             "bash-mcp:read_file",
		Provider:         "bash-mcp",
		PermissionObject: "hugen:tool:bash-mcp",
	}
	got, _, err := m.Resolve(context.Background(), tl, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.FromUser {
		t.Errorf("FromUser = true on Ask; want false")
	}
}

// TestToolManager_SetPolicies_RaceFreeWithResolve exercises the
// atomic.Pointer guard introduced for review B1: SetPolicies can
// swap the Tier-3 store concurrently with in-flight Resolve calls
// (e.g. through runtime_reload) without tripping `-race`. Before
// the fix m.policies was protected by m.mu only on the write path;
// Resolve dereferenced it lock-free.
func TestToolManager_SetPolicies_RaceFreeWithResolve(t *testing.T) {
	pol, _ := newPoliciesForTest(t)
	perms := &fakePermsWithAgent{agentID: "ag01"}
	m := NewToolManager(perms, nil, nil)
	tl := Tool{
		Name:             "bash-mcp:read_file",
		Provider:         "bash-mcp",
		PermissionObject: "hugen:tool:bash-mcp",
	}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 200; i++ {
			if i%2 == 0 {
				m.SetPolicies(pol)
			} else {
				m.SetPolicies(nil)
			}
		}
		close(done)
	}()
	for i := 0; i < 200; i++ {
		// We don't care about the outcome — only that the read
		// path is race-clean. Decide is a no-op when policies is
		// nil (IsConfigured false), so any swap is acceptable.
		_, _, _ = m.Resolve(context.Background(), tl, json.RawMessage(`{}`))
	}
	<-done
}

func TestToolManager_NewChild_DispatchWalksToParent(t *testing.T) {
	parent := NewToolManager(&fakePerms{}, nil, nil)
	root := &fakeProvider{name: "bash-mcp", callFunc: func(name string, _ json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`"root"`), nil
	}}
	if err := parent.AddProvider(root); err != nil {
		t.Fatalf("parent.AddProvider: %v", err)
	}

	child := parent.NewChild()
	own := &fakeProvider{name: "py-mcp", callFunc: func(name string, _ json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`"child"`), nil
	}}
	if err := child.AddProvider(own); err != nil {
		t.Fatalf("child.AddProvider: %v", err)
	}

	// Lookup hits child's own providers first.
	out, err := child.Dispatch(context.Background(),
		Tool{Name: "py-mcp:run", Provider: "py-mcp", PermissionObject: "hugen:tool:py-mcp"},
		json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("child Dispatch own: %v", err)
	}
	if string(out) != `"child"` {
		t.Errorf("child own dispatch = %s, want %q", out, `"child"`)
	}

	// Miss on child → walks to parent.
	out, err = child.Dispatch(context.Background(),
		Tool{Name: "bash-mcp:run", Provider: "bash-mcp", PermissionObject: "hugen:tool:bash-mcp"},
		json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("child Dispatch parent fallback: %v", err)
	}
	if string(out) != `"root"` {
		t.Errorf("parent fallback dispatch = %s, want %q", out, `"root"`)
	}

	// Unknown in both: ErrUnknownProvider, no panics.
	_, err = child.Dispatch(context.Background(),
		Tool{Name: "ghost:run", Provider: "ghost"}, nil)
	if !errors.Is(err, ErrUnknownProvider) {
		t.Errorf("child unknown dispatch = %v, want ErrUnknownProvider", err)
	}
}

func TestToolManager_NewChild_CloseDropsOwnOnly(t *testing.T) {
	parent := NewToolManager(&fakePerms{}, nil, nil)
	rootProv := &fakeProvider{name: "bash-mcp"}
	if err := parent.AddProvider(rootProv); err != nil {
		t.Fatalf("parent.AddProvider: %v", err)
	}

	child := parent.NewChild()
	childProv := &fakeProvider{name: "py-mcp"}
	if err := child.AddProvider(childProv); err != nil {
		t.Fatalf("child.AddProvider: %v", err)
	}

	if err := child.Close(); err != nil {
		t.Fatalf("child.Close: %v", err)
	}
	if !childProv.closed.Load() {
		t.Errorf("child.Close did not Close child's provider")
	}
	if rootProv.closed.Load() {
		t.Errorf("child.Close cascaded into parent's provider — must not")
	}
	if got := parent.Providers(); len(got) != 1 || got[0] != "bash-mcp" {
		t.Errorf("parent.Providers after child.Close = %v, want [bash-mcp]", got)
	}
}

func TestToolManager_NewChild_PoliciesInheritedFromParent(t *testing.T) {
	parent := NewToolManager(&fakePerms{}, nil, nil)
	pol := &Policies{}
	parent.SetPolicies(pol)

	child := parent.NewChild()
	if got := child.policiesSnapshot(); got != pol {
		t.Errorf("child.policiesSnapshot = %p, want parent's %p", got, pol)
	}
}

func TestToolManager_Reconnector_InheritedFromParent(t *testing.T) {
	parent := NewToolManager(&fakePerms{}, nil, nil)
	if parent.Reconnector() == nil {
		t.Fatal("parent.Reconnector should be non-nil after NewToolManager")
	}
	child := parent.NewChild()
	if child.Reconnector() != parent.Reconnector() {
		t.Errorf("child.Reconnector should walk to parent")
	}
}
