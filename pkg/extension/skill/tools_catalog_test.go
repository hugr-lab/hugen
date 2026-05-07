package skill

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/internal/fixture"
	skillpkg "github.com/hugr-lab/hugen/pkg/skill"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// fakeProvider exposes a fixed catalogue. Used to populate the
// test ToolManager so the tool_catalog handler has something to
// project.
type fakeProvider struct {
	name  string
	life  tool.Lifetime
	tools []tool.Tool
}

func (f *fakeProvider) Name() string                         { return f.name }
func (f *fakeProvider) Lifetime() tool.Lifetime              { return f.life }
func (f *fakeProvider) List(context.Context) ([]tool.Tool, error) { return f.tools, nil }
func (f *fakeProvider) Call(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	return nil, errors.New("fakeProvider: not callable")
}
func (f *fakeProvider) Subscribe(context.Context) (<-chan tool.ProviderEvent, error) {
	return nil, nil
}
func (f *fakeProvider) Close() error { return nil }

// catTestPerms allows every Resolve so ToolManager's snapshot
// path doesn't reject providers.
type catTestPerms struct{}

func (catTestPerms) Resolve(context.Context, string, string) (perm.Permission, error) {
	return perm.Permission{}, nil
}
func (catTestPerms) Refresh(context.Context) error { return nil }
func (catTestPerms) Subscribe(context.Context) (<-chan perm.RefreshEvent, error) {
	return nil, nil
}

// newCatalogFixture wires:
//   - SkillStore with an inline `alpha` skill granting `data-*` glob
//     on the `hugr-main` provider, plus a wildcard-free `beta`
//     skill granting only `hugr-main:discovery-list`;
//   - ToolManager with a `hugr-main` provider exposing two tools;
//   - skill Extension over the manager, with InitState run on a
//     TestSessionState that exposes the ToolManager via Tools().
func newCatalogFixture(t *testing.T, loadAlpha bool) (*Extension, *fixture.TestSessionState, *skillpkg.SkillManager) {
	t.Helper()
	store := skillpkg.NewSkillStore(skillpkg.Options{Inline: map[string][]byte{
		"alpha": []byte(`---
name: alpha
description: alpha grants data-* on hugr-main.
allowed-tools:
  - provider: hugr-main
    tools: [data-*]
compatibility:
  model: any
  runtime: hugen-phase-3
---
body
`),
		"beta": []byte(`---
name: beta
description: beta grants only discovery-list.
allowed-tools:
  - provider: hugr-main
    tools: [discovery-list]
compatibility:
  model: any
  runtime: hugen-phase-3
---
body
`),
	}})
	mgr := skillpkg.NewSkillManager(store, nil)

	tm := tool.NewToolManager(catTestPerms{}, nil, nil)
	prov := &fakeProvider{
		name: "hugr-main",
		life: tool.LifetimePerAgent,
		tools: []tool.Tool{
			{
				Name:             "hugr-main:discovery-list",
				Description:      "List data sources.",
				Provider:         "hugr-main",
				PermissionObject: "hugen:tool:hugr-main:discovery-list",
			},
			{
				Name:             "hugr-main:data-query",
				Description:      "Query data.",
				Provider:         "hugr-main",
				PermissionObject: "hugen:tool:hugr-main:data-query",
			},
		},
	}
	if err := tm.AddProvider(prov); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}

	ext := NewExtension(mgr, nil, "agent-cat")
	state := fixture.NewTestSessionState("ses-cat")
	state.SetTools(tm)
	if err := ext.InitState(context.Background(), state); err != nil {
		t.Fatalf("InitState: %v", err)
	}
	if loadAlpha {
		if err := mgr.Load(context.Background(), state.SessionID(), "alpha"); err != nil {
			t.Fatalf("Load alpha: %v", err)
		}
	}
	return ext, state, mgr
}

func TestToolsCatalog_RegisteredOnSkillProvider(t *testing.T) {
	ext, _, _ := newCatalogFixture(t, false)
	tools, err := ext.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, tt := range tools {
		if tt.Name == "skill:tools_catalog" {
			if tt.PermissionObject != permObjectToolsCatalog {
				t.Errorf("perm = %q, want %q", tt.PermissionObject, permObjectToolsCatalog)
			}
			return
		}
	}
	t.Errorf("skill:tools_catalog not registered on provider")
}

func TestToolsCatalog_GroupsAndFlagsGranted(t *testing.T) {
	ext, state, _ := newCatalogFixture(t, true)
	out, err := ext.Call(extension.WithSessionState(context.Background(), state),
		"skill:tools_catalog", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var got toolsCatalogResult
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	var hugr *toolsCatalogProvider
	for i := range got.Providers {
		if got.Providers[i].Name == "hugr-main" {
			hugr = &got.Providers[i]
		}
	}
	if hugr == nil {
		t.Fatalf("hugr-main missing: %s", out)
	}
	if hugr.Lifetime != "per_agent" {
		t.Errorf("lifetime = %q, want per_agent", hugr.Lifetime)
	}
	if len(hugr.Tools) != 2 {
		t.Fatalf("tools = %d, want 2", len(hugr.Tools))
	}
	// Sorted alphabetically: data-query first, then discovery-list.
	dq := hugr.Tools[0]
	dl := hugr.Tools[1]
	if dq.Name != "hugr-main:data-query" {
		t.Fatalf("entry[0] = %+v, want data-query first", dq)
	}
	if !dq.GrantedToSession {
		t.Errorf("data-query should be granted (alpha loaded with data-*): %+v", dq)
	}
	if !contains(dq.AvailableInSkills, "alpha") {
		t.Errorf("data-query AvailableInSkills = %v, want alpha", dq.AvailableInSkills)
	}
	if dl.Name != "hugr-main:discovery-list" {
		t.Fatalf("entry[1] = %+v, want discovery-list", dl)
	}
	if dl.GrantedToSession {
		t.Errorf("discovery-list should NOT be granted (alpha grants data-* only): %+v", dl)
	}
	if !contains(dl.AvailableInSkills, "beta") {
		t.Errorf("discovery-list AvailableInSkills = %v, must include beta", dl.AvailableInSkills)
	}
	if contains(dl.AvailableInSkills, "alpha") {
		t.Errorf("discovery-list AvailableInSkills includes alpha but alpha grants data-* only: %v", dl.AvailableInSkills)
	}
}

func TestToolsCatalog_ProviderFilter(t *testing.T) {
	ext, state, _ := newCatalogFixture(t, false)
	out, err := ext.Call(extension.WithSessionState(context.Background(), state),
		"skill:tools_catalog", json.RawMessage(`{"provider":"missing"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var got toolsCatalogResult
	_ = json.Unmarshal(out, &got)
	if len(got.Providers) != 0 {
		t.Errorf("providers = %d, want 0 (filter excludes hugr-main)", len(got.Providers))
	}
}

func TestToolsCatalog_PatternFilter(t *testing.T) {
	ext, state, _ := newCatalogFixture(t, false)
	out, err := ext.Call(extension.WithSessionState(context.Background(), state),
		"skill:tools_catalog", json.RawMessage(`{"pattern":"DISCOVERY"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var got toolsCatalogResult
	_ = json.Unmarshal(out, &got)
	if len(got.Providers) != 1 || len(got.Providers[0].Tools) != 1 {
		t.Fatalf("filtered providers/tools = %+v", got.Providers)
	}
	if !strings.Contains(got.Providers[0].Tools[0].Name, "discovery") {
		t.Errorf("tool = %q", got.Providers[0].Tools[0].Name)
	}
}

func TestToolsCatalog_BadRequest(t *testing.T) {
	ext, state, _ := newCatalogFixture(t, false)
	_, err := ext.Call(extension.WithSessionState(context.Background(), state),
		"skill:tools_catalog", json.RawMessage(`{not-json`))
	if err == nil {
		t.Fatalf("expected ErrArgValidation, got nil")
	}
	if !errors.Is(err, tool.ErrArgValidation) {
		t.Errorf("expected ErrArgValidation, got %v", err)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
