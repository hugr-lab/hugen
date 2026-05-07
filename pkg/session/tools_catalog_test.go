package session

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	skillext "github.com/hugr-lab/hugen/pkg/extension/skill"
	"github.com/hugr-lab/hugen/pkg/skill"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// fakeCatalogProvider is a minimal tool.ToolProvider exposing a
// fixed catalogue. Used to populate the per-session ToolManager so
// the tool_catalog handler has something to project.
type fakeCatalogProvider struct {
	name  string
	life  tool.Lifetime
	tools []tool.Tool
}

func (f *fakeCatalogProvider) Name() string         { return f.name }
func (f *fakeCatalogProvider) Lifetime() tool.Lifetime { return f.life }
func (f *fakeCatalogProvider) List(context.Context) ([]tool.Tool, error) {
	return f.tools, nil
}
func (f *fakeCatalogProvider) Call(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	return nil, errors.New("fakeCatalogProvider: not callable")
}
func (f *fakeCatalogProvider) Subscribe(context.Context) (<-chan tool.ProviderEvent, error) {
	return nil, nil
}
func (f *fakeCatalogProvider) Close() error { return nil }

// catalogTestPerms answers allow on every Resolve so the
// ToolManager's snapshot path doesn't reject the providers.
type catalogTestPerms struct{}

func (catalogTestPerms) Resolve(context.Context, string, string) (perm.Permission, error) {
	return perm.Permission{}, nil
}
func (catalogTestPerms) Refresh(context.Context) error { return nil }
func (catalogTestPerms) Subscribe(context.Context) (<-chan perm.RefreshEvent, error) {
	return nil, nil
}

// newCatalogFixture wires a session+ToolManager exposing one fake
// provider ("hugr-main" with two tools). Returns parent, the
// underlying ToolManager (for tests that need to add more
// providers), and cleanup.
func newCatalogFixture(t *testing.T, skills *skill.SkillManager) (*Session, *tool.ToolManager, func()) {
	t.Helper()
	tm := tool.NewToolManager(catalogTestPerms{}, nil, nil)
	prov := &fakeCatalogProvider{
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
	// Register the skill extension so its FilterTools narrows the
	// catalogue to bindings.AllowedTools — the path tool_catalog
	// reads via fetchSnapshot to fill `granted_to_session`. Pre
	// stage 2 the filter ran inside snapshot_cache directly off
	// s.skills; that's gone now.
	opts := []testParentOpt{withTestTools(tm), withTestSkills(skills)}
	if skills != nil {
		ext := skillext.NewExtension(skills, nil, "agent-cat")
		if err := tm.AddProvider(ext); err != nil {
			t.Fatalf("AddProvider skillext: %v", err)
		}
		opts = append(opts, withTestExtensions(ext))
	}
	parent, cleanup := newTestParent(t, opts...)
	return parent, tm, cleanup
}

func TestToolCatalog_GroupsAndFlagsGranted(t *testing.T) {
	// Inline skill granting only hugr-main:data-* glob.
	store := skill.NewSkillStore(skill.Options{Inline: map[string][]byte{
		"alpha": []byte(`---
name: alpha
description: alpha skill.
license: MIT
allowed-tools:
  - provider: hugr-main
    tools: [data-*]
---
body
`),
	}})
	skills := skill.NewSkillManager(store, nil)

	parent, _, cleanup := newCatalogFixture(t, skills)
	defer cleanup()
	if err := skills.Load(context.Background(), parent.ID(), "alpha"); err != nil {
		t.Fatalf("Load: %v", err)
	}

	out, err := parent.callToolCatalog(us1WithSession(parent), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var got toolCatalogResult
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v\nout=%s", err, out)
	}
	// Two providers — `hugr-main` (the test's fake) plus the
	// session-scoped `session` provider Session registers on its
	// own child manager. Locate hugr-main and run the grant
	// assertions against it.
	var hugr *toolCatalogProvider
	for i := range got.Providers {
		if got.Providers[i].Name == "hugr-main" {
			hugr = &got.Providers[i]
		}
	}
	if hugr == nil {
		t.Fatalf("hugr-main provider missing: %s", out)
	}
	if hugr.Lifetime != "per_agent" {
		t.Errorf("hugr-main lifetime = %q, want per_agent", hugr.Lifetime)
	}
	if len(hugr.Tools) != 2 {
		t.Fatalf("tools = %d, want 2", len(hugr.Tools))
	}
	// Sorted alphabetically: data-query first, then discovery-list.
	if hugr.Tools[0].Name != "hugr-main:data-query" || !hugr.Tools[0].GrantedToSession {
		t.Errorf("data-query entry = %+v", hugr.Tools[0])
	}
	if hugr.Tools[1].Name != "hugr-main:discovery-list" || hugr.Tools[1].GrantedToSession {
		t.Errorf("discovery-list entry = %+v (granted should be false)", hugr.Tools[1])
	}
}

func TestToolCatalog_ProviderFilter(t *testing.T) {
	skills := skill.NewSkillManager(skill.NewSkillStore(skill.Options{}), nil)
	parent, _, cleanup := newCatalogFixture(t, skills)
	defer cleanup()

	out, err := parent.callToolCatalog(us1WithSession(parent),
		json.RawMessage(`{"provider":"missing"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var got toolCatalogResult
	_ = json.Unmarshal(out, &got)
	if len(got.Providers) != 0 {
		t.Errorf("providers = %d, want 0 (filter excludes hugr-main)", len(got.Providers))
	}
}

func TestToolCatalog_PatternFilter(t *testing.T) {
	skills := skill.NewSkillManager(skill.NewSkillStore(skill.Options{}), nil)
	parent, _, cleanup := newCatalogFixture(t, skills)
	defer cleanup()

	out, err := parent.callToolCatalog(us1WithSession(parent),
		json.RawMessage(`{"pattern":"DISCOVERY"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var got toolCatalogResult
	_ = json.Unmarshal(out, &got)
	if len(got.Providers) != 1 || len(got.Providers[0].Tools) != 1 {
		t.Fatalf("filtered providers/tools = %+v", got.Providers)
	}
	if !strings.Contains(got.Providers[0].Tools[0].Name, "discovery") {
		t.Errorf("tool = %q", got.Providers[0].Tools[0].Name)
	}
}

func TestToolCatalog_BadRequest(t *testing.T) {
	skills := skill.NewSkillManager(skill.NewSkillStore(skill.Options{}), nil)
	parent, _, cleanup := newCatalogFixture(t, skills)
	defer cleanup()

	out, err := parent.callToolCatalog(us1WithSession(parent), json.RawMessage(`{not-json`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(string(out), `"code":"bad_request"`) {
		t.Errorf("expected bad_request, got %s", out)
	}
}

func TestToolCatalog_RegisteredOnSessionProvider(t *testing.T) {
	parent, cleanup := newTestParent(t)
	defer cleanup()
	tools, err := parent.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, tt := range tools {
		if tt.Name == "session:tool_catalog" {
			if tt.PermissionObject != permObjectToolCatalog {
				t.Errorf("perm = %q", tt.PermissionObject)
			}
			return
		}
	}
	t.Errorf("session:tool_catalog not registered on SessionToolProvider")
}
