package tool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
)

func TestSystemProvider_NameAndList(t *testing.T) {
	p := NewSystemProvider(SystemDeps{})
	if p.Name() != "system" {
		t.Errorf("Name = %q", p.Name())
	}
	if p.Lifetime() != LifetimePerAgent {
		t.Errorf("Lifetime = %v", p.Lifetime())
	}
	tools, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(tools) != 4 {
		t.Errorf("len(tools) = %d, want 4", len(tools))
	}
	for _, tt := range tools {
		if tt.Provider != "system" {
			t.Errorf("Tool %s provider = %q", tt.Name, tt.Provider)
		}
		if tt.PermissionObject != "hugen:tool:system" {
			t.Errorf("Tool %s perm = %q", tt.Name, tt.PermissionObject)
		}
		if !strings.HasPrefix(tt.Name, "system:") {
			t.Errorf("Tool %s missing prefix", tt.Name)
		}
	}
}

func TestSystemProvider_RuntimeReload_RoutesTarget(t *testing.T) {
	got := ""
	p := NewSystemProvider(SystemDeps{
		Reload: func(ctx context.Context, target string) error {
			got = target
			return nil
		},
	})
	if _, err := p.Call(context.Background(), "runtime_reload", json.RawMessage(`{"target":"permissions"}`)); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got != "permissions" {
		t.Errorf("target = %q", got)
	}
}

func TestSystemProvider_RuntimeReload_DefaultAll(t *testing.T) {
	got := ""
	p := NewSystemProvider(SystemDeps{
		Reload: func(ctx context.Context, target string) error {
			got = target
			return nil
		},
	})
	if _, err := p.Call(context.Background(), "runtime_reload", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got != "all" {
		t.Errorf("target = %q, want all", got)
	}
}

func TestSystemProvider_RuntimeReload_BadTarget(t *testing.T) {
	called := 0
	p := NewSystemProvider(SystemDeps{
		Reload: func(context.Context, string) error { called++; return nil },
	})
	_, err := p.Call(context.Background(), "runtime_reload", json.RawMessage(`{"target":"bogus"}`))
	if !errors.Is(err, ErrArgValidation) {
		t.Errorf("err = %v, want ErrArgValidation", err)
	}
	if called != 0 {
		t.Errorf("Reload called %d times despite bad target", called)
	}
}

func TestSystemProvider_RuntimeReload_GateDenied(t *testing.T) {
	called := 0
	perms := &fakePerms{rules: map[string]perm.Permission{
		"hugen:command:runtime_reload:mcp": {Disabled: true, FromConfig: true},
	}}
	p := NewSystemProvider(SystemDeps{
		Perms:  perms,
		Reload: func(context.Context, string) error { called++; return nil },
	})
	_, err := p.Call(context.Background(), "runtime_reload", json.RawMessage(`{"target":"mcp"}`))
	if !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("err = %v, want ErrPermissionDenied", err)
	}
	if called != 0 {
		t.Errorf("Reload invoked %d times despite gate deny", called)
	}
	// Different target stays allowed.
	if _, err := p.Call(context.Background(), "runtime_reload", json.RawMessage(`{"target":"skills"}`)); err != nil {
		t.Errorf("non-denied target failed: %v", err)
	}
	if called != 1 {
		t.Errorf("Reload calls = %d, want 1", called)
	}
}

func TestSystemProvider_MCPAdd_RoutesSpec(t *testing.T) {
	var captured MCPAddSpec
	p := NewSystemProvider(SystemDeps{
		AddMCP: func(ctx context.Context, spec MCPAddSpec) error {
			captured = spec
			return nil
		},
	})
	args := json.RawMessage(`{"name":"web","command":"web-mcp","args":["--port","9000"],"env":{"K":"v"}}`)
	if _, err := p.Call(context.Background(), "mcp_add_server", args); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if captured.Name != "web" || captured.Command != "web-mcp" {
		t.Errorf("spec = %+v", captured)
	}
	if len(captured.Args) != 2 || captured.Env["K"] != "v" {
		t.Errorf("spec args/env = %+v", captured)
	}
}

func TestSystemProvider_MCPAdd_MissingFields(t *testing.T) {
	p := NewSystemProvider(SystemDeps{AddMCP: func(ctx context.Context, spec MCPAddSpec) error { return nil }})
	_, err := p.Call(context.Background(), "mcp_add_server", json.RawMessage(`{"name":"web"}`))
	if !errors.Is(err, ErrArgValidation) {
		t.Errorf("err = %v, want ErrArgValidation", err)
	}
}

func TestSystemProvider_MCPRemove_RoutesName(t *testing.T) {
	got := ""
	p := NewSystemProvider(SystemDeps{
		RemoveMCP: func(ctx context.Context, name string) error {
			got = name
			return nil
		},
	})
	if _, err := p.Call(context.Background(), "mcp_remove_server", json.RawMessage(`{"name":"web"}`)); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got != "web" {
		t.Errorf("name = %q", got)
	}
}

func TestSystemProvider_PermissionGate_DispatchedThroughManager(t *testing.T) {
	// Tier-1 floor disables system:mcp_add_server. ToolManager.Resolve
	// must surface ErrPermissionDenied; SystemProvider.Call is never
	// reached.
	deps := SystemDeps{
		AddMCP: func(ctx context.Context, spec MCPAddSpec) error {
			t.Errorf("AddMCP must not be invoked when permission denies")
			return nil
		},
	}
	sp := NewSystemProvider(deps)
	perms := &fakePerms{rules: map[string]perm.Permission{
		"hugen:tool:system:mcp_add_server": {Disabled: true, FromConfig: true},
	}}
	m := NewToolManager(perms, nil, nil)
	if err := m.AddProvider(sp); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	tool := Tool{
		Name:             "system:mcp_add_server",
		Provider:         "system",
		PermissionObject: "hugen:tool:system",
	}
	_, _, err := m.Resolve(context.Background(), tool, json.RawMessage(`{"name":"x","command":"y"}`))
	if !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("err = %v, want ErrPermissionDenied", err)
	}
}

func TestSystemProvider_UnknownTool(t *testing.T) {
	p := NewSystemProvider(SystemDeps{})
	_, err := p.Call(context.Background(), "ghost", json.RawMessage(`{}`))
	if !errors.Is(err, ErrUnknownTool) {
		t.Errorf("err = %v, want ErrUnknownTool", err)
	}
}



