package perm

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/identity"
)

// fakeView is a minimal PermissionsView for tests.
type fakeView struct {
	rules           []Rule
	refreshInterval time.Duration
	remoteEnabled   bool
	updateCallbacks []func()
}

func (f *fakeView) Rules() []Rule                  { return f.rules }
func (f *fakeView) RefreshInterval() time.Duration { return f.refreshInterval }
func (f *fakeView) RemoteEnabled() bool            { return f.remoteEnabled }
func (f *fakeView) OnUpdate(fn func()) (cancel func()) {
	f.updateCallbacks = append(f.updateCallbacks, fn)
	idx := len(f.updateCallbacks) - 1
	return func() {
		f.updateCallbacks = append(f.updateCallbacks[:idx], f.updateCallbacks[idx+1:]...)
	}
}
func (f *fakeView) fire() {
	for _, cb := range f.updateCallbacks {
		cb()
	}
}

// fakeIdentity is a tiny identity.Source for tests. Agent.ID is
// the only field LocalPermissions reads.
type fakeIdentity struct{ id string }

func (f fakeIdentity) Agent(_ context.Context) (identity.Agent, error) {
	return identity.Agent{ID: f.id, Name: f.id}, nil
}
func (f fakeIdentity) WhoAmI(_ context.Context) (identity.WhoAmI, error) {
	return identity.WhoAmI{UserID: f.id, UserName: f.id, Role: "test"}, nil
}
func (f fakeIdentity) Permission(_ context.Context, _, _ string) (identity.Permission, error) {
	return identity.Permission{Enabled: true}, nil
}

func newPerms(t *testing.T, v PermissionsView, agentID string) *LocalPermissions {
	t.Helper()
	return NewLocalPermissions(v, fakeIdentity{id: agentID})
}

func TestLocalPermissions_NilView(t *testing.T) {
	l := NewLocalPermissions(nil, fakeIdentity{id: "a1"})
	defer l.Close()
	p, err := l.Resolve(context.Background(), "type", "field")
	if err != nil {
		t.Fatalf("Resolve(nil view) error: %v", err)
	}
	if p.Disabled || p.FromConfig {
		t.Errorf("Permission = %+v, want zero", p)
	}
}

func TestLocalPermissions_DisabledFloor(t *testing.T) {
	v := &fakeView{rules: []Rule{
		{Type: "hugen:tool:bash-mcp", Field: "bash.write_file", Disabled: true},
	}}
	l := newPerms(t, v, "a1")
	defer l.Close()
	p, err := l.Resolve(context.Background(), "hugen:tool:bash-mcp", "bash.write_file")
	if err != nil {
		t.Fatalf("Resolve error: %v", err)
	}
	if !p.Disabled {
		t.Errorf("Permission.Disabled = false, want true (Tier-1 floor)")
	}
	if !p.FromConfig {
		t.Errorf("Permission.FromConfig = false, want true")
	}
}

func TestLocalPermissions_WildcardThenExactSpecificityWins(t *testing.T) {
	v := &fakeView{rules: []Rule{
		// Wildcard sets Hidden, exact rule clears it via Disabled.
		{Type: "hugen:tool:bash-mcp", Field: "*", Hidden: true},
		{Type: "hugen:tool:bash-mcp", Field: "bash.run", Disabled: true},
	}}
	l := newPerms(t, v, "a1")
	defer l.Close()

	p, _ := l.Resolve(context.Background(), "hugen:tool:bash-mcp", "bash.run")
	if !p.Hidden {
		t.Errorf("p.Hidden = false, want true (from wildcard)")
	}
	if !p.Disabled {
		t.Errorf("p.Disabled = false, want true (from exact)")
	}

	p2, _ := l.Resolve(context.Background(), "hugen:tool:bash-mcp", "bash.read_file")
	if !p2.Hidden {
		t.Errorf("p2.Hidden = false, want true (from wildcard)")
	}
	if p2.Disabled {
		t.Errorf("p2.Disabled = true, want false (no exact match)")
	}
}

func TestLocalPermissions_DataMergeAndTemplate(t *testing.T) {
	v := &fakeView{rules: []Rule{
		{
			Type:  "hugen:tool:bash-mcp",
			Field: "*",
			Data:  json.RawMessage(`{"workspace": "/var/agents/[$agent.id]/workspace"}`),
		},
		{
			Type:  "hugen:tool:bash-mcp",
			Field: "bash.run",
			Data:  json.RawMessage(`{"timeout_ms": 5000}`),
		},
	}}
	l := newPerms(t, v, "agent-7")
	defer l.Close()

	p, err := l.Resolve(context.Background(), "hugen:tool:bash-mcp", "bash.run")
	if err != nil {
		t.Fatalf("Resolve error: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(p.Data, &got); err != nil {
		t.Fatalf("Data not valid JSON: %v\n%s", err, p.Data)
	}
	if got["workspace"] != "/var/agents/agent-7/workspace" {
		t.Errorf("workspace = %v, want /var/agents/agent-7/workspace (template substituted)", got["workspace"])
	}
	if got["timeout_ms"].(float64) != 5000 {
		t.Errorf("timeout_ms = %v, want 5000", got["timeout_ms"])
	}
}

func TestLocalPermissions_FilterAndMerge(t *testing.T) {
	v := &fakeView{rules: []Rule{
		{Type: "hugen:tool:hugr-query", Field: "*", Filter: "user_id = '[$auth.user_id]'"},
		{Type: "hugen:tool:hugr-query", Field: "hugr.Query", Filter: "active = true"},
	}}
	l := newPerms(t, v, "agent-7")
	defer l.Close()

	p, _ := l.Resolve(context.Background(), "hugen:tool:hugr-query", "hugr.Query")
	want := "(user_id = 'agent-7') AND (active = true)"
	if p.Filter != want {
		t.Errorf("Filter = %q\nwant: %q", p.Filter, want)
	}
}

func TestLocalPermissions_NoMatchReturnsZero(t *testing.T) {
	v := &fakeView{rules: []Rule{
		{Type: "hugen:tool:other", Field: "*", Disabled: true},
	}}
	l := newPerms(t, v, "a1")
	defer l.Close()
	p, _ := l.Resolve(context.Background(), "hugen:tool:bash-mcp", "bash.run")
	if p.FromConfig {
		t.Errorf("FromConfig = true, want false (no rule matched)")
	}
	if p.Disabled {
		t.Errorf("Disabled = true, want false (no rule matched)")
	}
}

func TestLocalPermissions_OnUpdateTriggersResnapshot(t *testing.T) {
	v := &fakeView{rules: []Rule{}}
	l := newPerms(t, v, "a1")
	defer l.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := l.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe error: %v", err)
	}

	v.rules = []Rule{{Type: "hugen:tool:bash-mcp", Field: "*", Disabled: true}}
	v.fire()

	select {
	case ev := <-ch:
		if ev.Generation == 0 {
			t.Errorf("RefreshEvent.Generation = 0, want non-zero")
		}
	case <-time.After(time.Second):
		t.Fatal("RefreshEvent not delivered after OnUpdate fired")
	}

	p, _ := l.Resolve(ctx, "hugen:tool:bash-mcp", "bash.run")
	if !p.Disabled {
		t.Errorf("Permission.Disabled = false, want true (snapshot updated)")
	}
}

func TestLocalPermissions_ContextCancel(t *testing.T) {
	v := &fakeView{rules: []Rule{}}
	l := newPerms(t, v, "a1")
	defer l.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := l.Resolve(ctx, "x", "y")
	if err == nil {
		t.Fatal("Resolve(cancelled ctx) returned nil error")
	}
}

func TestLocalPermissions_SessionMetadataFromContext(t *testing.T) {
	v := &fakeView{rules: []Rule{
		{
			Type:  "hugen:tool:bash-mcp",
			Field: "*",
			Data:  json.RawMessage(`{"path": "/workspace/[$session.id]/data"}`),
		},
	}}
	l := newPerms(t, v, "a1")
	defer l.Close()
	ctx := WithSession(context.Background(), SessionContext{SessionID: "ses-42"})
	p, err := l.Resolve(ctx, "hugen:tool:bash-mcp", "bash.read_file")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(p.Data, &got)
	if got["path"] != "/workspace/ses-42/data" {
		t.Errorf("path = %v, want /workspace/ses-42/data", got["path"])
	}
}
