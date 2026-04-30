package tool

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hugr-lab/query-engine/types"
)

// fakePolicyStore is a thin in-memory backing for a `types.Querier`
// that recognises the small handful of GraphQL operations
// pkg/tool/policies.go runs against the local store. Other engine
// methods panic — they're never touched in these tests.
type fakePolicyStore struct {
	rows map[string]map[string]string // composite key → projection
}

type fakePolicyQuerier struct {
	store *fakePolicyStore
	err   error // fail-next-call hook; reset after each consumed Query
}

func newFakePolicyStore() *fakePolicyStore {
	return &fakePolicyStore{rows: map[string]map[string]string{}}
}

func newFakePolicyQuerier(s *fakePolicyStore) *fakePolicyQuerier {
	return &fakePolicyQuerier{store: s}
}

func (f *fakePolicyQuerier) Query(ctx context.Context, query string, vars map[string]any) (*types.Response, error) {
	_ = ctx
	if f.err != nil {
		err := f.err
		f.err = nil
		return nil, err
	}
	switch {
	case strings.Contains(query, "insert_tool_policies"):
		return f.insert(vars)
	case strings.Contains(query, "update_tool_policies"):
		return f.update(vars)
	case strings.Contains(query, "delete_tool_policies"):
		return f.delete(vars)
	case strings.Contains(query, "tool_policies(filter"):
		return f.list(vars)
	}
	return nil, errors.New("fakePolicyQuerier: unrecognised query")
}

func (f *fakePolicyQuerier) Subscribe(ctx context.Context, query string, vars map[string]any) (*types.Subscription, error) {
	panic("not implemented")
}
func (f *fakePolicyQuerier) RegisterDataSource(ctx context.Context, ds types.DataSource) error {
	panic("not implemented")
}
func (f *fakePolicyQuerier) LoadDataSource(ctx context.Context, name string) error {
	panic("not implemented")
}
func (f *fakePolicyQuerier) UnloadDataSource(ctx context.Context, name string, opts ...types.UnloadOpt) error {
	panic("not implemented")
}
func (f *fakePolicyQuerier) DataSourceStatus(ctx context.Context, name string) (string, error) {
	panic("not implemented")
}
func (f *fakePolicyQuerier) DescribeDataSource(ctx context.Context, name string, self bool) (string, error) {
	panic("not implemented")
}

func keyOf(agentID, toolName, scope string) string {
	return agentID + "|" + toolName + "|" + scope
}

func (f *fakePolicyQuerier) list(vars map[string]any) (*types.Response, error) {
	agent, _ := vars["agent"].(string)
	rows := []map[string]any{}
	for _, r := range f.store.rows {
		if r["agent_id"] != agent {
			continue
		}
		rows = append(rows, map[string]any{
			"agent_id":  r["agent_id"],
			"tool_name": r["tool_name"],
			"scope":     r["scope"],
			"policy":    r["policy"],
			"note":      r["note"],
		})
	}
	return &types.Response{Data: map[string]any{
		"hub": map[string]any{
			"db": map[string]any{
				"agent": map[string]any{
					"tool_policies": rows,
				},
			},
		},
	}}, nil
}

func (f *fakePolicyQuerier) insert(vars map[string]any) (*types.Response, error) {
	data, _ := vars["data"].(map[string]any)
	agent, _ := data["agent_id"].(string)
	tool, _ := data["tool_name"].(string)
	scope, _ := data["scope"].(string)
	policy, _ := data["policy"].(string)
	note, _ := data["note"].(string)
	createdBy, _ := data["created_by"].(string)
	k := keyOf(agent, tool, scope)
	if _, ok := f.store.rows[k]; ok {
		return nil, errors.New("fakePolicyQuerier: duplicate insert")
	}
	f.store.rows[k] = map[string]string{
		"agent_id":   agent,
		"tool_name":  tool,
		"scope":      scope,
		"policy":     policy,
		"note":       note,
		"created_by": createdBy,
	}
	return &types.Response{Data: map[string]any{}}, nil
}

func (f *fakePolicyQuerier) update(vars map[string]any) (*types.Response, error) {
	agent, _ := vars["agent"].(string)
	tool, _ := vars["tool"].(string)
	scope, _ := vars["scope"].(string)
	data, _ := vars["data"].(map[string]any)
	k := keyOf(agent, tool, scope)
	r, ok := f.store.rows[k]
	if !ok {
		return &types.Response{Data: map[string]any{
			"hub": map[string]any{
				"db": map[string]any{
					"agent": map[string]any{
						"update_tool_policies": map[string]any{"affected_rows": 0},
					},
				},
			},
		}}, nil
	}
	if v, ok := data["policy"].(string); ok {
		r["policy"] = v
	}
	if v, ok := data["note"].(string); ok {
		r["note"] = v
	}
	return &types.Response{Data: map[string]any{
		"hub": map[string]any{
			"db": map[string]any{
				"agent": map[string]any{
					"update_tool_policies": map[string]any{"affected_rows": 1},
				},
			},
		},
	}}, nil
}

func (f *fakePolicyQuerier) delete(vars map[string]any) (*types.Response, error) {
	agent, _ := vars["agent"].(string)
	tool, _ := vars["tool"].(string)
	scope, _ := vars["scope"].(string)
	delete(f.store.rows, keyOf(agent, tool, scope))
	return &types.Response{Data: map[string]any{}}, nil
}

func TestPolicies_SaveRoundTrip(t *testing.T) {
	store := newFakePolicyStore()
	p := NewPolicies(newFakePolicyQuerier(store))
	ctx := context.Background()

	id, err := p.Save(ctx, PolicyInput{
		AgentID:   "ag01",
		ToolName:  "bash-mcp:read_file",
		Decision:  PolicyAllow,
		CreatedBy: PolicyCreatorUser,
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if id == "" {
		t.Fatalf("save: empty id")
	}
	if got := len(store.rows); got != 1 {
		t.Fatalf("rows after save = %d, want 1", got)
	}
	row := store.rows[id]
	if row["policy"] != "always_allowed" {
		t.Fatalf("policy = %q, want always_allowed", row["policy"])
	}
	if row["scope"] != PolicyScopeGlobal {
		t.Fatalf("scope = %q, want %q", row["scope"], PolicyScopeGlobal)
	}
}

func TestPolicies_SaveUpsertsExisting(t *testing.T) {
	store := newFakePolicyStore()
	p := NewPolicies(newFakePolicyQuerier(store))
	ctx := context.Background()

	in := PolicyInput{
		AgentID:  "ag01",
		ToolName: "bash-mcp:read_file",
		Decision: PolicyAllow,
		Note:     "initial",
	}
	if _, err := p.Save(ctx, in); err != nil {
		t.Fatalf("first save: %v", err)
	}
	if got := len(store.rows); got != 1 {
		t.Fatalf("rows after first = %d", got)
	}
	in.Decision = PolicyDeny
	in.Note = "second"
	id, err := p.Save(ctx, in)
	if err != nil {
		t.Fatalf("second save: %v", err)
	}
	if got := len(store.rows); got != 1 {
		t.Fatalf("rows after upsert = %d, want 1", got)
	}
	row := store.rows[id]
	if row["policy"] != "denied" {
		t.Fatalf("policy = %q after upsert, want denied", row["policy"])
	}
	if row["note"] != "second" {
		t.Fatalf("note = %q after upsert, want %q", row["note"], "second")
	}
}

func TestPolicies_RevokeDeletesRow(t *testing.T) {
	store := newFakePolicyStore()
	p := NewPolicies(newFakePolicyQuerier(store))
	ctx := context.Background()
	id, err := p.Save(ctx, PolicyInput{
		AgentID:  "ag01",
		ToolName: "bash-mcp:read_file",
		Decision: PolicyAllow,
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := p.Revoke(ctx, id); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if got := len(store.rows); got != 0 {
		t.Fatalf("rows after revoke = %d, want 0", got)
	}
	// Idempotent on already-removed row.
	if err := p.Revoke(ctx, id); err != nil {
		t.Fatalf("revoke twice: %v", err)
	}
}

func TestPolicies_RevokeMalformedID(t *testing.T) {
	p := NewPolicies(newFakePolicyQuerier(newFakePolicyStore()))
	if err := p.Revoke(context.Background(), "not-a-composite"); err == nil {
		t.Fatalf("expected error for malformed id")
	}
}

func TestPolicies_DecideAllowExact(t *testing.T) {
	store := newFakePolicyStore()
	p := NewPolicies(newFakePolicyQuerier(store))
	ctx := context.Background()
	if _, err := p.Save(ctx, PolicyInput{
		AgentID:  "ag01",
		ToolName: "bash-mcp:read_file",
		Decision: PolicyAllow,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := p.Decide(ctx, "ag01", "bash-mcp:read_file", "")
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if got.Outcome != PolicyAllow {
		t.Fatalf("outcome = %v, want PolicyAllow", got.Outcome)
	}
	if got.ToolName != "bash-mcp:read_file" {
		t.Fatalf("tool = %q", got.ToolName)
	}
	if got.Scope != PolicyScopeGlobal {
		t.Fatalf("scope = %q", got.Scope)
	}
}

func TestPolicies_DecideDenyWins(t *testing.T) {
	store := newFakePolicyStore()
	p := NewPolicies(newFakePolicyQuerier(store))
	ctx := context.Background()
	if _, err := p.Save(ctx, PolicyInput{
		AgentID:  "ag01",
		ToolName: "bash-mcp:write_file",
		Decision: PolicyDeny,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := p.Decide(ctx, "ag01", "bash-mcp:write_file", "")
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if got.Outcome != PolicyDeny {
		t.Fatalf("outcome = %v, want PolicyDeny", got.Outcome)
	}
}

func TestPolicies_DecideAskOnNoMatch(t *testing.T) {
	store := newFakePolicyStore()
	p := NewPolicies(newFakePolicyQuerier(store))
	ctx := context.Background()
	got, err := p.Decide(ctx, "ag01", "bash-mcp:write_file", "")
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if got.Outcome != PolicyAsk {
		t.Fatalf("outcome = %v, want PolicyAsk", got.Outcome)
	}
}

func TestPolicies_DecidePrefixGlob(t *testing.T) {
	store := newFakePolicyStore()
	p := NewPolicies(newFakePolicyQuerier(store))
	ctx := context.Background()
	if _, err := p.Save(ctx, PolicyInput{
		AgentID:  "ag01",
		ToolName: "hugr-main:data-*",
		Decision: PolicyAllow,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	for _, tc := range []struct {
		name string
		full string
		want PolicyOutcome
	}{
		{"data prefix", "hugr-main:data-execute_query", PolicyAllow},
		{"different prefix", "hugr-main:discovery-search_data_sources", PolicyAsk},
		{"other provider", "bash-mcp:data-execute_query", PolicyAsk},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := p.Decide(ctx, "ag01", tc.full, "")
			if err != nil {
				t.Fatalf("decide: %v", err)
			}
			if got.Outcome != tc.want {
				t.Fatalf("outcome = %v, want %v", got.Outcome, tc.want)
			}
		})
	}
}

func TestPolicies_DecideExactBeatsPrefix(t *testing.T) {
	store := newFakePolicyStore()
	p := NewPolicies(newFakePolicyQuerier(store))
	ctx := context.Background()
	if _, err := p.Save(ctx, PolicyInput{
		AgentID:  "ag01",
		ToolName: "hugr-main:data-*",
		Decision: PolicyAllow,
	}); err != nil {
		t.Fatalf("save prefix: %v", err)
	}
	if _, err := p.Save(ctx, PolicyInput{
		AgentID:  "ag01",
		ToolName: "hugr-main:data-execute_query",
		Decision: PolicyDeny,
	}); err != nil {
		t.Fatalf("save exact: %v", err)
	}
	got, err := p.Decide(ctx, "ag01", "hugr-main:data-execute_query", "")
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if got.Outcome != PolicyDeny {
		t.Fatalf("exact deny should win, got %v", got.Outcome)
	}
}

func TestPolicies_DecidePerAgentIsolation(t *testing.T) {
	store := newFakePolicyStore()
	p := NewPolicies(newFakePolicyQuerier(store))
	ctx := context.Background()
	if _, err := p.Save(ctx, PolicyInput{
		AgentID:  "ag01",
		ToolName: "bash-mcp:read_file",
		Decision: PolicyAllow,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := p.Decide(ctx, "ag02", "bash-mcp:read_file", "")
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if got.Outcome != PolicyAsk {
		t.Fatalf("expected Ask for other agent, got %v", got.Outcome)
	}
}

func TestPolicies_DecideScopeChain(t *testing.T) {
	store := newFakePolicyStore()
	p := NewPolicies(newFakePolicyQuerier(store))
	ctx := context.Background()
	// global says deny, role says allow.
	if _, err := p.Save(ctx, PolicyInput{
		AgentID:  "ag01",
		ToolName: "bash-mcp:read_file",
		Scope:    PolicyScopeGlobal,
		Decision: PolicyDeny,
	}); err != nil {
		t.Fatalf("global save: %v", err)
	}
	if _, err := p.Save(ctx, PolicyInput{
		AgentID:  "ag01",
		ToolName: "bash-mcp:read_file",
		Scope:    "role:hugr-data:analyst",
		Decision: PolicyAllow,
	}); err != nil {
		t.Fatalf("role save: %v", err)
	}
	got, err := p.Decide(ctx, "ag01", "bash-mcp:read_file", "role:hugr-data:analyst")
	if err != nil {
		t.Fatalf("decide role: %v", err)
	}
	if got.Outcome != PolicyAllow {
		t.Fatalf("role-scoped should win, got %v", got.Outcome)
	}
	// Global-scoped lookup ignores the role row.
	got, err = p.Decide(ctx, "ag01", "bash-mcp:read_file", "")
	if err != nil {
		t.Fatalf("decide global: %v", err)
	}
	if got.Outcome != PolicyDeny {
		t.Fatalf("global-scoped should deny, got %v", got.Outcome)
	}
}

func TestPolicies_NilStoreNoop(t *testing.T) {
	var p *Policies
	if p.IsConfigured() {
		t.Fatalf("nil receiver should not be configured")
	}
	got, err := p.Decide(context.Background(), "ag01", "bash-mcp:read_file", "")
	if err != nil {
		t.Fatalf("decide on nil: %v", err)
	}
	if got.Outcome != PolicyAsk {
		t.Fatalf("nil decide should return Ask, got %v", got.Outcome)
	}
}

func TestPolicies_DecideUnknownPolicyValueErrors(t *testing.T) {
	store := newFakePolicyStore()
	store.rows[keyOf("ag01", "bash-mcp:read_file", PolicyScopeGlobal)] = map[string]string{
		"agent_id":  "ag01",
		"tool_name": "bash-mcp:read_file",
		"scope":     PolicyScopeGlobal,
		"policy":    "garbage",
	}
	p := NewPolicies(newFakePolicyQuerier(store))
	if _, err := p.Decide(context.Background(), "ag01", "bash-mcp:read_file", ""); err == nil {
		t.Fatalf("expected error on garbage policy value")
	}
}
