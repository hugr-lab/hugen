package session

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/identity"
	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// integration_us4_test.go covers exit criteria 8, 9, 10, 12 from
// design/001-agent-runtime/phase-3-spec.md §11:
//
//   8. Tier-2 (Hugr role) Disabled denial blocks the call.
//   9. Tier-1 (config floor) wins over Tier-2 silence — if the
//      floor disables a tool, no role rule can re-enable it.
//   10. Data injection: a role rule's data field is merged into
//      the tool args; LLM-supplied conflicting values are
//      overridden in the audit Frame.
//   12. TTL refresh + runtime_reload: a Tier-2 ruleset change
//      becomes effective after the next refresh tick or an
//      explicit runtime_reload call.

// us4View is a minimal PermissionsView that holds a stable rule
// list (the Tier-1 floor) plus per-test refresh interval / remote
// flag.
type us4View struct {
	rules           []perm.Rule
	refreshInterval time.Duration
}

func (v *us4View) Rules() []perm.Rule             { return v.rules }
func (v *us4View) RefreshInterval() time.Duration { return v.refreshInterval }
func (v *us4View) RemoteEnabled() bool            { return true }
func (v *us4View) OnUpdate(_ func()) func()       { return func() {} }

// us4Querier is a stub perm.Querier with mutable rule list and a
// call counter so we can assert refresh behaviour.
type us4Querier struct {
	mu    atomicRules
	calls atomic.Int64
}

type atomicRules struct {
	v atomic.Value // []perm.Rule
}

func (a *atomicRules) Set(r []perm.Rule) { a.v.Store(append([]perm.Rule(nil), r...)) }
func (a *atomicRules) Get() []perm.Rule {
	r, _ := a.v.Load().([]perm.Rule)
	return r
}

func newUS4Querier(initial []perm.Rule) *us4Querier {
	q := &us4Querier{}
	q.mu.Set(initial)
	return q
}

func (q *us4Querier) QueryRules(_ context.Context) ([]perm.Rule, error) {
	q.calls.Add(1)
	return q.mu.Get(), nil
}

// us4FakeIdentity satisfies identity.Source for the test
// permission service. UserID/Role come back from WhoAmI; AgentID
// from Agent.
type us4FakeIdentity struct{ id, role string }

func (i us4FakeIdentity) Agent(_ context.Context) (identity.Agent, error) {
	return identity.Agent{ID: i.id, Name: i.id}, nil
}
func (i us4FakeIdentity) WhoAmI(_ context.Context) (identity.WhoAmI, error) {
	return identity.WhoAmI{UserID: i.id, UserName: i.id, Role: i.role}, nil
}
func (us4FakeIdentity) Permission(context.Context, string, string) (identity.Permission, error) {
	return identity.Permission{Enabled: true}, nil
}

// us4Stub is the fake "fake:do" provider; tracks args observed
// after permission resolution so the test can assert injected
// data values.
type us4Stub struct {
	tools    []tool.Tool
	result   string
	lastArgs json.RawMessage
	calls    int
}

func (p *us4Stub) Name() string                                                 { return "fake" }
func (p *us4Stub) Lifetime() tool.Lifetime                                      { return tool.LifetimePerAgent }
func (p *us4Stub) List(context.Context) ([]tool.Tool, error)                    { return p.tools, nil }
func (p *us4Stub) Subscribe(context.Context) (<-chan tool.ProviderEvent, error) { return nil, nil }
func (p *us4Stub) Close() error                                                 { return nil }
func (p *us4Stub) Call(_ context.Context, _ string, args json.RawMessage) (json.RawMessage, error) {
	p.calls++
	p.lastArgs = append(json.RawMessage(nil), args...)
	return json.RawMessage(p.result), nil
}

func us4NewSession(t *testing.T, mdl model.Model, perms perm.Service, agentID string, providers ...tool.ToolProvider) (*Session, *tool.ToolManager, context.CancelFunc) {
	t.Helper()
	store := newFakeStore()
	_ = store.OpenSession(context.Background(), SessionRow{ID: "s1", AgentID: agentID, Status: StatusActive})

	tm := tool.NewToolManager(perms, nil, nil)
	for _, p := range providers {
		if err := tm.AddProvider(p); err != nil {
			t.Fatalf("AddProvider: %v", err)
		}
	}
	router := newRouterWithModel(t, mdl)
	agent, err := NewAgent(agentID, "hugen", &fakeIdentity{id: agentID}, "")
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	sess := NewSession("s1", agent, store, router, NewCommandRegistry(), protocol.NewCodec(), nil, WithTools(tm))
	sess.materialised.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = sess.Run(ctx) }()
	return sess, tm, cancel
}

// TestUS4_Tier2DisabledBlocks — Tier-2 (Hugr role) disables the
// tool; Resolve must surface permission_denied. Exit-criterion 8.
func TestUS4_Tier2DisabledBlocks(t *testing.T) {
	view := &us4View{refreshInterval: time.Hour}
	q := newUS4Querier([]perm.Rule{
		{Type: "hugen:tool:fake", Field: "do", Disabled: true},
	})
	perms := perm.NewRemotePermissions(view, us4FakeIdentity{id: "ag01", role: "analyst"}, q)

	provider := &us4Stub{
		tools: []tool.Tool{{
			Name: "fake:do", Provider: "fake", PermissionObject: "hugen:tool:fake",
		}},
		result: `{"ok":true}`,
	}
	mdl := &scriptedToolModel{turns: [][]model.Chunk{
		{{ToolCall: &model.ChunkToolCall{ID: "tc", Name: "fake:do"}}},
		{{Content: ptr("done"), Final: true}},
	}}
	sess, _, cancel := us4NewSession(t, mdl, perms, "ag01", provider)
	defer cancel()

	user := protocol.ParticipantInfo{ID: "u1", Kind: protocol.ParticipantUser}
	sess.Inbox() <- protocol.NewUserMessage("s1", user, "go")

	frames := collectFrames(t, sess, func(seen []protocol.Frame) bool {
		if am, ok := seen[len(seen)-1].(*protocol.AgentMessage); ok && am.Payload.Final {
			return true
		}
		return false
	}, 3*time.Second)

	if provider.calls != 0 {
		t.Errorf("provider invoked %d times despite Tier-2 deny", provider.calls)
	}
	var sawDenied bool
	for _, f := range frames {
		if tr, ok := f.(*protocol.ToolResult); ok && tr.Payload.IsError {
			body, _ := json.Marshal(tr.Payload.Result)
			if strings.Contains(string(body), protocol.ToolErrorPermissionDenied) {
				sawDenied = true
			}
		}
	}
	if !sawDenied {
		t.Errorf("missing permission_denied result")
	}
}

// TestUS4_Tier1FloorBeatsTier2Silence — operator floor disables
// fake:do; Tier-2 stays silent (no rule). Floor still wins.
// Exit-criterion 9.
func TestUS4_Tier1FloorBeatsTier2Silence(t *testing.T) {
	view := &us4View{
		rules:           []perm.Rule{{Type: "hugen:tool:fake", Field: "do", Disabled: true}},
		refreshInterval: time.Hour,
	}
	q := newUS4Querier(nil) // remote silent
	perms := perm.NewRemotePermissions(view, us4FakeIdentity{id: "ag01", role: "analyst"}, q)

	provider := &us4Stub{
		tools: []tool.Tool{{
			Name: "fake:do", Provider: "fake", PermissionObject: "hugen:tool:fake",
		}},
		result: `{"ok":true}`,
	}
	mdl := &scriptedToolModel{turns: [][]model.Chunk{
		{{ToolCall: &model.ChunkToolCall{ID: "tc", Name: "fake:do"}}},
		{{Content: ptr("done"), Final: true}},
	}}
	sess, _, cancel := us4NewSession(t, mdl, perms, "ag01", provider)
	defer cancel()

	user := protocol.ParticipantInfo{ID: "u1", Kind: protocol.ParticipantUser}
	sess.Inbox() <- protocol.NewUserMessage("s1", user, "go")

	frames := collectFrames(t, sess, func(seen []protocol.Frame) bool {
		if am, ok := seen[len(seen)-1].(*protocol.AgentMessage); ok && am.Payload.Final {
			return true
		}
		return false
	}, 3*time.Second)

	if provider.calls != 0 {
		t.Errorf("provider called %d; floor should block", provider.calls)
	}
	for _, f := range frames {
		if tr, ok := f.(*protocol.ToolResult); ok && tr.Payload.IsError {
			return
		}
	}
	t.Errorf("expected an error tool_result")
}

// TestUS4_DataInjection — Tier-2 rule injects {"tenant_id":7}
// into args; LLM-supplied "tenant_id":99 is overridden because
// rule data wins. Exit-criterion 10.
func TestUS4_DataInjection(t *testing.T) {
	view := &us4View{refreshInterval: time.Hour}
	q := newUS4Querier([]perm.Rule{{
		Type:  "hugen:tool:fake",
		Field: "do",
		Data:  json.RawMessage(`{"tenant_id":7}`),
	}})
	perms := perm.NewRemotePermissions(view, us4FakeIdentity{id: "ag01", role: "analyst"}, q)

	provider := &us4Stub{
		tools: []tool.Tool{{
			Name: "fake:do", Provider: "fake", PermissionObject: "hugen:tool:fake",
		}},
		result: `{"ok":true}`,
	}
	mdl := &scriptedToolModel{turns: [][]model.Chunk{
		{{ToolCall: &model.ChunkToolCall{
			ID: "tc", Name: "fake:do",
			Args: map[string]any{"tenant_id": 99, "limit": 10},
		}}},
		{{Content: ptr("done"), Final: true}},
	}}
	sess, _, cancel := us4NewSession(t, mdl, perms, "ag01", provider)
	defer cancel()

	user := protocol.ParticipantInfo{ID: "u1", Kind: protocol.ParticipantUser}
	sess.Inbox() <- protocol.NewUserMessage("s1", user, "go")

	collectFrames(t, sess, func(seen []protocol.Frame) bool {
		if am, ok := seen[len(seen)-1].(*protocol.AgentMessage); ok && am.Payload.Final {
			return true
		}
		return false
	}, 3*time.Second)

	if provider.calls != 1 {
		t.Fatalf("provider calls = %d, want 1", provider.calls)
	}
	var got map[string]any
	if err := json.Unmarshal(provider.lastArgs, &got); err != nil {
		t.Fatalf("unmarshal effective args: %v", err)
	}
	if v, ok := got["tenant_id"].(float64); !ok || int(v) != 7 {
		t.Errorf("tenant_id = %v, want 7 (rule wins over LLM-supplied 99)", got["tenant_id"])
	}
	if v, ok := got["limit"].(float64); !ok || int(v) != 10 {
		t.Errorf("limit = %v, want 10 (LLM-supplied retained when no rule conflict)", got["limit"])
	}
}

// TestUS4_TTLRefreshAndRuntimeReload — initial Tier-2 allows
// fake:do; remote flips to deny; an explicit Refresh (mimicking
// runtime_reload) picks up the change before TTL elapses.
// Exit-criterion 12.
func TestUS4_TTLRefreshAndRuntimeReload(t *testing.T) {
	view := &us4View{refreshInterval: time.Hour}
	q := newUS4Querier(nil) // start permissive
	perms := perm.NewRemotePermissions(view, us4FakeIdentity{id: "ag01", role: "analyst"}, q)

	// Force a snapshot load so the next refresh observes a real
	// change instead of being the first fetch.
	if _, err := perms.Resolve(context.Background(), "hugen:tool:fake", "do"); err != nil {
		t.Fatalf("seed resolve: %v", err)
	}
	if got := q.calls.Load(); got != 1 {
		t.Fatalf("seed calls = %d, want 1", got)
	}

	// Upstream tightens the rule.
	q.mu.Set([]perm.Rule{{Type: "hugen:tool:fake", Field: "do", Disabled: true}})

	// Without refresh, the cached snapshot still allows.
	got, err := perms.Resolve(context.Background(), "hugen:tool:fake", "do")
	if err != nil {
		t.Fatalf("pre-refresh resolve: %v", err)
	}
	if got.Disabled {
		t.Errorf("pre-refresh saw upstream change despite TTL not elapsed")
	}

	// runtime_reload routes target=permissions to perms.Refresh.
	if err := perms.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if q.calls.Load() < 2 {
		t.Fatalf("expected ≥ 2 fetch calls after refresh, got %d", q.calls.Load())
	}

	got, err = perms.Resolve(context.Background(), "hugen:tool:fake", "do")
	if err != nil {
		t.Fatalf("post-refresh resolve: %v", err)
	}
	if !got.Disabled {
		t.Errorf("post-refresh did not pick up new rule")
	}
}

// TestUS4_RefreshFailureKeepsSnapshot — a transient remote
// failure does not flip the cached decision; the previous
// snapshot continues to serve until a later refresh succeeds.
func TestUS4_RefreshFailureKeepsSnapshot(t *testing.T) {
	view := &us4View{refreshInterval: 30 * time.Millisecond}
	q := newUS4Querier([]perm.Rule{
		{Type: "hugen:tool:fake", Field: "do", Disabled: true},
	})
	perms := perm.NewRemotePermissions(view, us4FakeIdentity{id: "ag01", role: "analyst"}, q)

	// Initial resolve loads the snapshot.
	got, err := perms.Resolve(context.Background(), "hugen:tool:fake", "do")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if !got.Disabled {
		t.Fatalf("seed must see disabled")
	}

	// Sabotage the next refresh with a stub that returns an
	// error. We accomplish this by wrapping the Querier.
	wrapped := &errorOnceQuerier{inner: q}
	view.refreshInterval = 30 * time.Millisecond
	perms2 := perm.NewRemotePermissions(view, us4FakeIdentity{id: "ag01", role: "analyst"}, wrapped)
	// seed
	if _, err := perms2.Resolve(context.Background(), "hugen:tool:fake", "do"); err != nil {
		t.Fatalf("seed2: %v", err)
	}
	wrapped.fail = errors.New("hugr unreachable")
	time.Sleep(40 * time.Millisecond) // age past TTL

	got, err = perms2.Resolve(context.Background(), "hugen:tool:fake", "do")
	if err != nil {
		t.Fatalf("Resolve after refresh failure: %v", err)
	}
	if !got.Disabled {
		t.Errorf("Disabled flipped despite refresh failure preserving snapshot")
	}
}

type errorOnceQuerier struct {
	inner *us4Querier
	fail  error
}

func (q *errorOnceQuerier) QueryRules(ctx context.Context) ([]perm.Rule, error) {
	if q.fail != nil {
		err := q.fail
		q.fail = nil
		return nil, err
	}
	return q.inner.QueryRules(ctx)
}
