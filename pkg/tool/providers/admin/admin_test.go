package admin

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// fakePerms / fakeProvider mirror the test fixtures used by
// pkg/tool — kept here so the admin tests stand alone.
type fakePerms struct{}

func (f *fakePerms) Resolve(context.Context, string, string) (perm.Permission, error) {
	return perm.Permission{}, nil
}
func (f *fakePerms) Refresh(context.Context) error { return nil }
func (f *fakePerms) Subscribe(context.Context) (<-chan perm.RefreshEvent, error) {
	return nil, nil
}

type fakeProvider struct {
	name string
}

func (f *fakeProvider) Name() string                                      { return f.name }
func (f *fakeProvider) Lifetime() tool.Lifetime                           { return tool.LifetimePerAgent }
func (f *fakeProvider) List(context.Context) ([]tool.Tool, error)         { return nil, nil }
func (f *fakeProvider) Call(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	return nil, nil
}
func (f *fakeProvider) Subscribe(context.Context) (<-chan tool.ProviderEvent, error) {
	return nil, nil
}
func (f *fakeProvider) Close() error { return nil }

// fakeBuilder produces a fakeProvider whose name matches Spec.Name
// — enough to exercise the admin path without wiring a real MCP.
type fakeBuilder struct{ built []string }

func (b *fakeBuilder) Build(_ context.Context, spec tool.Spec) (tool.ToolProvider, error) {
	b.built = append(b.built, spec.Name)
	return &fakeProvider{name: spec.Name}, nil
}

func newManager(t *testing.T, b tool.ProviderBuilder) *tool.ToolManager {
	t.Helper()
	return tool.NewToolManager(&fakePerms{}, nil, nil, nil,
		slog.New(slog.DiscardHandler), tool.WithBuilder(b))
}

func TestAdminProvider_List_NamesAndPermissions(t *testing.T) {
	m := newManager(t, &fakeBuilder{})
	a := New(m)
	tools, err := a.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("List len = %d, want 2", len(tools))
	}
	want := map[string]string{
		"tool:provider_add":    "hugen:tool:provider_add",
		"tool:provider_remove": "hugen:tool:provider_remove",
	}
	for _, tl := range tools {
		got, ok := want[tl.Name]
		if !ok {
			t.Errorf("unexpected tool %q", tl.Name)
			continue
		}
		if tl.PermissionObject != got {
			t.Errorf("%s PermissionObject = %q, want %q", tl.Name, tl.PermissionObject, got)
		}
		if tl.Provider != "tool" {
			t.Errorf("%s Provider = %q, want tool", tl.Name, tl.Provider)
		}
		if err := tool.ValidateLLMSchema(tl.ArgSchema); err != nil {
			t.Errorf("%s schema invalid: %v", tl.Name, err)
		}
	}
}

func TestAdminProvider_AddRemove_RoundTrip(t *testing.T) {
	b := &fakeBuilder{}
	m := newManager(t, b)
	a := New(m)

	// Add
	out, err := a.Call(context.Background(), "provider_add",
		json.RawMessage(`{"name":"py-mcp","type":"mcp","transport":"stdio","command":"python"}`))
	if err != nil {
		t.Fatalf("provider_add: %v", err)
	}
	if !contains(string(out), `"py-mcp"`) {
		t.Errorf("add result = %s", out)
	}
	if got := m.Providers(); len(got) != 1 || got[0] != "py-mcp" {
		t.Errorf("after add Providers = %v", got)
	}
	if len(b.built) != 1 || b.built[0] != "py-mcp" {
		t.Errorf("Builder.built = %v", b.built)
	}

	// Remove
	out, err = a.Call(context.Background(), "provider_remove",
		json.RawMessage(`{"name":"py-mcp"}`))
	if err != nil {
		t.Fatalf("provider_remove: %v", err)
	}
	if !contains(string(out), `"py-mcp"`) {
		t.Errorf("remove result = %s", out)
	}
	if got := m.Providers(); len(got) != 0 {
		t.Errorf("after remove Providers = %v", got)
	}
}

func TestAdminProvider_Add_RequiresName(t *testing.T) {
	a := New(newManager(t, &fakeBuilder{}))
	_, err := a.Call(context.Background(), "provider_add",
		json.RawMessage(`{"type":"mcp"}`))
	if !errors.Is(err, tool.ErrArgValidation) {
		t.Errorf("err = %v, want ErrArgValidation", err)
	}
}

func TestAdminProvider_UnknownToolReturnsError(t *testing.T) {
	a := New(newManager(t, &fakeBuilder{}))
	_, err := a.Call(context.Background(), "ghost", json.RawMessage(`{}`))
	if !errors.Is(err, tool.ErrUnknownTool) {
		t.Errorf("err = %v, want ErrUnknownTool", err)
	}
}

func TestAdminProvider_AddBySpec_ErrorPropagates(t *testing.T) {
	// Manager without a wired Builder returns ErrBuilderNotConfigured
	// from AddBySpec — admin must propagate it verbatim.
	m := tool.NewToolManager(&fakePerms{}, nil, nil, nil,
		slog.New(slog.DiscardHandler))
	a := New(m)
	_, err := a.Call(context.Background(), "provider_add",
		json.RawMessage(`{"name":"x","type":"mcp"}`))
	if !errors.Is(err, tool.ErrBuilderNotConfigured) {
		t.Errorf("err = %v, want ErrBuilderNotConfigured", err)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (sub == "" || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
