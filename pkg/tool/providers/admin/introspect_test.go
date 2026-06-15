package admin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hugr-lab/hugen/pkg/tool"
)

// toolfulProvider returns a fixed set of tools so the introspection
// surface has something to enumerate.
type toolfulProvider struct {
	name  string
	tools []tool.Tool
}

func (p *toolfulProvider) Name() string            { return p.name }
func (p *toolfulProvider) Lifetime() tool.Lifetime { return tool.LifetimePerAgent }
func (p *toolfulProvider) List(context.Context) ([]tool.Tool, error) {
	return p.tools, nil
}
func (p *toolfulProvider) Call(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	return nil, nil
}
func (p *toolfulProvider) Subscribe(context.Context) (<-chan tool.ProviderEvent, error) {
	return nil, nil
}
func (p *toolfulProvider) Close() error { return nil }

func newToolfulManager(t *testing.T) *tool.ToolManager {
	t.Helper()
	m := newManager(t, &fakeBuilder{})
	prov := &toolfulProvider{
		name: "hugr-main",
		tools: []tool.Tool{
			{
				Name:        "hugr-main:data-inline_graphql_result",
				Description: "Run a GraphQL query and inline the result. Use for small reads.",
				Provider:    "hugr-main",
				ArgSchema:   json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`),
			},
			{
				Name:        "hugr-main:schema-type_fields",
				Description: "List the fields of a type.",
				Provider:    "hugr-main",
			},
		},
	}
	if err := m.AddProvider(prov); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	return m
}

func TestIntrospect_Providers(t *testing.T) {
	a := New(newToolfulManager(t))
	out, err := a.Call(context.Background(), "providers", nil)
	if err != nil {
		t.Fatalf("providers: %v", err)
	}
	var res struct {
		Providers []providerEntry `json:"providers"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var found *providerEntry
	for i := range res.Providers {
		if res.Providers[i].Name == "hugr-main" {
			found = &res.Providers[i]
		}
	}
	if found == nil {
		t.Fatalf("hugr-main not in providers: %+v", res.Providers)
	}
	if found.ToolCount != 2 {
		t.Errorf("hugr-main tool_count = %d, want 2", found.ToolCount)
	}
}

func TestIntrospect_Tools_BriefAndFull(t *testing.T) {
	a := New(newToolfulManager(t))

	// Brief: name + summary, NO argument schema.
	out, err := a.Call(context.Background(), "tools", json.RawMessage(`{"provider":"hugr-main"}`))
	if err != nil {
		t.Fatalf("tools brief: %v", err)
	}
	var brief struct {
		Tools []toolEntryBrief `json:"tools"`
	}
	if err := json.Unmarshal(out, &brief); err != nil {
		t.Fatalf("unmarshal brief: %v", err)
	}
	if len(brief.Tools) != 2 {
		t.Fatalf("brief tools = %d, want 2", len(brief.Tools))
	}
	if brief.Tools[0].Summary == "" {
		t.Error("brief summary empty")
	}

	// Full: includes the argument schema.
	out, err = a.Call(context.Background(), "tools", json.RawMessage(`{"provider":"hugr-main","detailed":true}`))
	if err != nil {
		t.Fatalf("tools full: %v", err)
	}
	var full struct {
		Tools []toolEntryFull `json:"tools"`
	}
	if err := json.Unmarshal(out, &full); err != nil {
		t.Fatalf("unmarshal full: %v", err)
	}
	var graphql *toolEntryFull
	for i := range full.Tools {
		if full.Tools[i].Name == "hugr-main:data-inline_graphql_result" {
			graphql = &full.Tools[i]
		}
	}
	if graphql == nil {
		t.Fatal("graphql tool missing from full listing")
	}
	if len(graphql.Arguments) == 0 {
		t.Error("full listing missing argument schema")
	}

	// Pattern filter narrows the set.
	out, err = a.Call(context.Background(), "tools", json.RawMessage(`{"provider":"hugr-main","pattern":"schema"}`))
	if err != nil {
		t.Fatalf("tools pattern: %v", err)
	}
	_ = json.Unmarshal(out, &brief)
	if len(brief.Tools) != 1 || brief.Tools[0].Name != "hugr-main:schema-type_fields" {
		t.Errorf("pattern=schema = %+v, want only schema-type_fields", brief.Tools)
	}
}

func TestIntrospect_Tools_UnknownProvider(t *testing.T) {
	a := New(newToolfulManager(t))
	_, err := a.Call(context.Background(), "tools", json.RawMessage(`{"provider":"nope"}`))
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
}
