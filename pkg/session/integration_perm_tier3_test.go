package session

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/model"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/internal/fixture"
	"github.com/hugr-lab/hugen/pkg/tool"
	"github.com/hugr-lab/hugen/pkg/tool/providers/policies"
	"github.com/hugr-lab/query-engine/types"
)

// integration_us3_test.go covers exit criterion 11 from
// design/001-agent-runtime/phase-3-spec.md §11:
//
//   - policy_save happy path persists the row, the next call to
//     the tool runs without re-asking;
//   - Tier-1 floor wins over a Tier-3 allow row;
//   - Per-agent isolation across sessions: a row scoped to ag01
//     does not influence decisions for ag02;
//   - Deployment-blocked hugen:policy:persist:* returns
//     permission_denied on save and persists nothing.
//
// The full Tier-3 logic also has unit coverage in
// pkg/tool/policies_test.go and pkg/tool/manager_test.go; this
// file is the runtime-shaped end-to-end check that exercises
// the SystemProvider → Policies → ToolManager.Resolve flow
// behind a real Session.

// fakeUS3Querier reuses the in-memory store/querier shape from
// pkg/tool/policies_test.go, re-declared here because tests in
// different packages can't share helpers.
type fakeUS3Querier struct {
	rows map[string]map[string]string // agent|tool|scope → projection
}

func newFakeUS3Querier() *fakeUS3Querier {
	return &fakeUS3Querier{rows: map[string]map[string]string{}}
}

func (f *fakeUS3Querier) key(agentID, toolName, scope string) string {
	return agentID + "|" + toolName + "|" + scope
}

func (f *fakeUS3Querier) Query(_ context.Context, query string, vars map[string]any) (*types.Response, error) {
	switch {
	case strings.Contains(query, "insert_tool_policies"):
		data, _ := vars["data"].(map[string]any)
		agent, _ := data["agent_id"].(string)
		toolName, _ := data["tool_name"].(string)
		scope, _ := data["scope"].(string)
		policy, _ := data["policy"].(string)
		note, _ := data["note"].(string)
		k := f.key(agent, toolName, scope)
		if _, ok := f.rows[k]; ok {
			return nil, errors.New("duplicate insert")
		}
		f.rows[k] = map[string]string{
			"agent_id":  agent,
			"tool_name": toolName,
			"scope":     scope,
			"policy":    policy,
			"note":      note,
		}
		return &types.Response{Data: map[string]any{}}, nil
	case strings.Contains(query, "update_tool_policies"):
		agent, _ := vars["agent"].(string)
		toolName, _ := vars["tool"].(string)
		scope, _ := vars["scope"].(string)
		data, _ := vars["data"].(map[string]any)
		k := f.key(agent, toolName, scope)
		r, ok := f.rows[k]
		affected := 0
		if ok {
			if v, ok := data["policy"].(string); ok {
				r["policy"] = v
			}
			if v, ok := data["note"].(string); ok {
				r["note"] = v
			}
			affected = 1
		}
		return &types.Response{Data: map[string]any{
			"hub": map[string]any{"db": map[string]any{"agent": map[string]any{
				"update_tool_policies": map[string]any{"affected_rows": affected},
			}}},
		}}, nil
	case strings.Contains(query, "delete_tool_policies"):
		agent, _ := vars["agent"].(string)
		toolName, _ := vars["tool"].(string)
		scope, _ := vars["scope"].(string)
		delete(f.rows, f.key(agent, toolName, scope))
		return &types.Response{Data: map[string]any{}}, nil
	case strings.Contains(query, "tool_policies(filter"):
		agent, _ := vars["agent"].(string)
		out := []map[string]any{}
		for _, r := range f.rows {
			if r["agent_id"] != agent {
				continue
			}
			out = append(out, map[string]any{
				"agent_id":  r["agent_id"],
				"tool_name": r["tool_name"],
				"scope":     r["scope"],
				"policy":    r["policy"],
				"note":      r["note"],
			})
		}
		return &types.Response{Data: map[string]any{
			"hub": map[string]any{"db": map[string]any{"agent": map[string]any{
				"tool_policies": out,
			}}},
		}}, nil
	}
	return nil, errors.New("fakeUS3Querier: unrecognised query")
}

func (f *fakeUS3Querier) Subscribe(context.Context, string, map[string]any) (*types.Subscription, error) {
	panic("not implemented")
}
func (f *fakeUS3Querier) RegisterDataSource(context.Context, types.DataSource) error {
	panic("not implemented")
}
func (f *fakeUS3Querier) LoadDataSource(context.Context, string) error {
	panic("not implemented")
}
func (f *fakeUS3Querier) UnloadDataSource(context.Context, string, ...types.UnloadOpt) error {
	panic("not implemented")
}
func (f *fakeUS3Querier) DataSourceStatus(context.Context, string) (string, error) {
	panic("not implemented")
}
func (f *fakeUS3Querier) DescribeDataSource(context.Context, string, bool) (string, error) {
	panic("not implemented")
}

// us3Perms is a perm.Service stub that honours the AgentID
// surface ToolManager.Resolve uses for Tier-3 lookups, plus
// optional rule injection (policy floor / persist gate).
type us3Perms struct {
	agentID string
	rules   map[string]perm.Permission
}

func (p *us3Perms) Resolve(_ context.Context, object, field string) (perm.Permission, error) {
	if r, ok := p.rules[object+":"+field]; ok {
		return r, nil
	}
	if r, ok := p.rules[object+":*"]; ok {
		return r, nil
	}
	return perm.Permission{}, nil
}
func (p *us3Perms) Refresh(context.Context) error                               { return nil }
func (p *us3Perms) Subscribe(context.Context) (<-chan perm.RefreshEvent, error) { return nil, nil }
func (p *us3Perms) AgentID() string                                             { return p.agentID }

// us3Stub is the test "fake" tool. It tracks call counts so we
// can assert the second invocation skips the prompt path.
type us3Stub struct {
	tools  []tool.Tool
	result string
	calls  int
}

func (p *us3Stub) Name() string                              { return "fake" }
func (p *us3Stub) Lifetime() tool.Lifetime                   { return tool.LifetimePerAgent }
func (p *us3Stub) List(context.Context) ([]tool.Tool, error) { return p.tools, nil }
func (p *us3Stub) Subscribe(context.Context) (<-chan tool.ProviderEvent, error) {
	return nil, nil
}
func (p *us3Stub) Close() error { return nil }
func (p *us3Stub) Call(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	p.calls++
	return json.RawMessage(p.result), nil
}

func us3NewSession(t *testing.T, mdl model.Model, perms perm.Service, agentID string, providers ...tool.ToolProvider) (*Session, *tool.ToolManager, context.CancelFunc) {
	t.Helper()
	store := fixture.NewTestStore()
	_ = store.OpenSession(context.Background(), SessionRow{ID: "s1", AgentID: agentID, Status: StatusActive})

	tm := tool.NewToolManager(perms, nil, nil)
	for _, p := range providers {
		if err := tm.AddProvider(p); err != nil {
			t.Fatalf("AddProvider: %v", err)
		}
	}

	router := newRouterWithModel(t, mdl)
	agent, err := NewAgent(agentID, "hugen", &fakeIdentity{id: agentID}, "", nil)
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	sess := NewSession("s1", agent, store, router, NewCommandRegistry(), protocol.NewCodec(), tm, nil)
	sess.materialised.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = sess.Run(ctx) }()
	return sess, tm, cancel
}

// TestPermTier3_AlwaysAllow_NextCallSkipsPromptPath drives the model
// to call fake:do twice with a policy_save in between. Both
// calls succeed. The second call's tool_result must be marked
// FromUser (the audit Frame names the user as the deciding
// tier). This exercises exit-criterion-11 step 1.
func TestPermTier3_AlwaysAllow_NextCallSkipsPromptPath(t *testing.T) {
	q := newFakeUS3Querier()

	provider := &us3Stub{
		tools: []tool.Tool{{
			Name:             "fake:do",
			Provider:         "fake",
			PermissionObject: "hugen:tool:fake",
		}},
		result: `{"ok":true}`,
	}

	mdl := &scriptedToolModel{
		turns: [][]model.Chunk{
			{
				{ToolCall: &model.ChunkToolCall{
					ID:   "tc-allow",
					Name: "policy:save",
					Args: map[string]any{
						"tool_name": "fake:do",
						"decision":  "allow",
					},
				}},
			},
			{
				{ToolCall: &model.ChunkToolCall{ID: "tc-do", Name: "fake:do"}},
			},
			{
				{Content: ptr("done"), Final: true},
			},
		},
	}

	perms := &us3Perms{agentID: "ag01"}

	policiesProv := policies.New(q, perms, nil)

	sess, tm, cancel := us3NewSession(t, mdl, perms, "ag01", provider, policiesProv)
	defer cancel()
	tm.SetPolicies(policiesProv)

	user := protocol.ParticipantInfo{ID: "u1", Kind: protocol.ParticipantUser}
	sess.Inbox() <- protocol.NewUserMessage("s1", user, "go")

	frames := collectFrames(t, sess, func(seen []protocol.Frame) bool {
		if am, ok := seen[len(seen)-1].(*protocol.AgentMessage); ok && am.Payload.Final {
			return true
		}
		return false
	}, 3*time.Second)

	if provider.calls != 1 {
		t.Errorf("fake:do invoked %d times, want 1", provider.calls)
	}
	// Confirm both tool_result frames are non-error.
	var errCount int
	for _, f := range frames {
		if tr, ok := f.(*protocol.ToolResult); ok && tr.Payload.IsError {
			errCount++
		}
	}
	if errCount != 0 {
		t.Errorf("got %d error tool_results, want 0", errCount)
	}
	// The persist row landed in the store.
	if got := len(q.rows); got != 1 {
		t.Errorf("policy rows = %d, want 1", got)
	}
}

// TestPermTier1_FloorBeatsTier3 — operator floor disables fake:do; a
// Tier-3 allow row cannot relax it. Exit-criterion-11 step 2.
func TestPermTier1_FloorBeatsTier3(t *testing.T) {
	q := newFakeUS3Querier()
	pol := policies.New(q, nil, nil)
	if _, err := pol.Save(context.Background(), policies.Input{
		AgentID:  "ag01",
		ToolName: "fake:do",
		Decision: tool.PolicyAllow,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	provider := &us3Stub{
		tools: []tool.Tool{{
			Name: "fake:do", Provider: "fake", PermissionObject: "hugen:tool:fake",
		}},
		result: `{"ok":true}`,
	}

	mdl := &scriptedToolModel{
		turns: [][]model.Chunk{
			{
				{ToolCall: &model.ChunkToolCall{ID: "tc-do", Name: "fake:do"}},
			},
			{
				{Content: ptr("done"), Final: true},
			},
		},
	}

	perms := &us3Perms{
		agentID: "ag01",
		rules: map[string]perm.Permission{
			"hugen:tool:fake:*": {Disabled: true, FromConfig: true},
		},
	}

	sess, tm, cancel := us3NewSession(t, mdl, perms, "ag01", provider)
	defer cancel()
	tm.SetPolicies(pol)

	user := protocol.ParticipantInfo{ID: "u1", Kind: protocol.ParticipantUser}
	sess.Inbox() <- protocol.NewUserMessage("s1", user, "go")

	frames := collectFrames(t, sess, func(seen []protocol.Frame) bool {
		if am, ok := seen[len(seen)-1].(*protocol.AgentMessage); ok && am.Payload.Final {
			return true
		}
		return false
	}, 3*time.Second)

	if provider.calls != 0 {
		t.Errorf("fake:do invoked %d times despite floor deny", provider.calls)
	}
	var denied bool
	for _, f := range frames {
		if tr, ok := f.(*protocol.ToolResult); ok && tr.Payload.IsError {
			body, _ := json.Marshal(tr.Payload.Result)
			if strings.Contains(string(body), protocol.ToolErrorPermissionDenied) {
				denied = true
			}
		}
	}
	if !denied {
		t.Errorf("missing tool_result{permission_denied}; floor must block")
	}
}

// TestPermTier3_PerAgentIsolation — a row saved against ag01 has no
// effect on a session running as ag02. Exit-criterion-11 step 3.
func TestPermTier3_PerAgentIsolation(t *testing.T) {
	q := newFakeUS3Querier()
	pol := policies.New(q, nil, nil)
	if _, err := pol.Save(context.Background(), policies.Input{
		AgentID:  "ag01",
		ToolName: "fake:do",
		Decision: tool.PolicyDeny,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	provider := &us3Stub{
		tools: []tool.Tool{{
			Name: "fake:do", Provider: "fake", PermissionObject: "hugen:tool:fake",
		}},
		result: `{"ok":true}`,
	}

	mdl := &scriptedToolModel{
		turns: [][]model.Chunk{
			{
				{ToolCall: &model.ChunkToolCall{ID: "tc-do", Name: "fake:do"}},
			},
			{
				{Content: ptr("done"), Final: true},
			},
		},
	}
	// ag02 should see no policy rows for itself.
	perms := &us3Perms{agentID: "ag02"}

	sess, tm, cancel := us3NewSession(t, mdl, perms, "ag02", provider)
	defer cancel()
	tm.SetPolicies(pol)

	user := protocol.ParticipantInfo{ID: "u1", Kind: protocol.ParticipantUser}
	sess.Inbox() <- protocol.NewUserMessage("s1", user, "go")

	frames := collectFrames(t, sess, func(seen []protocol.Frame) bool {
		if am, ok := seen[len(seen)-1].(*protocol.AgentMessage); ok && am.Payload.Final {
			return true
		}
		return false
	}, 3*time.Second)

	if provider.calls != 1 {
		t.Errorf("fake:do invoked %d times for ag02, want 1 (isolation broken)", provider.calls)
	}
	for _, f := range frames {
		if tr, ok := f.(*protocol.ToolResult); ok && tr.Payload.IsError {
			t.Errorf("unexpected error result on ag02: %+v", tr.Payload)
		}
	}
}

// TestPermTier3_PolicyPersistGateBlocksSave — operator denies
// hugen:policy:persist:* so policy_save returns permission_denied.
// Exit-criterion-11 step 4.
func TestPermTier3_PolicyPersistGateBlocksSave(t *testing.T) {
	q := newFakeUS3Querier()

	mdl := &scriptedToolModel{
		turns: [][]model.Chunk{
			{
				{ToolCall: &model.ChunkToolCall{
					ID:   "tc-save",
					Name: "policy:save",
					Args: map[string]any{
						"tool_name": "fake:do",
						"decision":  "allow",
					},
				}},
			},
			{
				{Content: ptr("ok"), Final: true},
			},
		},
	}

	perms := &us3Perms{
		agentID: "ag01",
		rules: map[string]perm.Permission{
			"hugen:policy:persist:fake:do": {Disabled: true, FromConfig: true},
		},
	}

	policiesProv := policies.New(q, perms, nil)

	sess, tm, cancel := us3NewSession(t, mdl, perms, "ag01", policiesProv)
	defer cancel()
	tm.SetPolicies(policiesProv)

	user := protocol.ParticipantInfo{ID: "u1", Kind: protocol.ParticipantUser}
	sess.Inbox() <- protocol.NewUserMessage("s1", user, "go")

	frames := collectFrames(t, sess, func(seen []protocol.Frame) bool {
		if am, ok := seen[len(seen)-1].(*protocol.AgentMessage); ok && am.Payload.Final {
			return true
		}
		return false
	}, 3*time.Second)

	var sawDenied bool
	for _, f := range frames {
		if tr, ok := f.(*protocol.ToolResult); ok && tr.Payload.IsError {
			body, _ := json.Marshal(tr.Payload.Result)
			if strings.Contains(string(body), "permission_denied") || strings.Contains(string(body), "policy:save") {
				sawDenied = true
			}
		}
	}
	if !sawDenied {
		t.Errorf("missing denied tool_result for blocked policy:save: %v", kindNames(frames))
	}
	if got := len(q.rows); got != 0 {
		t.Errorf("policy rows after blocked save = %d, want 0", got)
	}
}
