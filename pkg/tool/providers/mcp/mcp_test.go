package mcp

import (
	"context"
	"testing"

	"github.com/hugr-lab/hugen/pkg/tool"
)

// New rejects a stdio spec missing the Command — the projected
// legacy MCPProviderSpec fails BuildMCPProviderSpec validation,
// which surfaces as an error from New (no provider returned, no
// teardown to run).
func TestNew_RejectsStdioWithoutCommand(t *testing.T) {
	_, err := New(context.Background(), tool.Spec{
		Name: "broken",
		Type: "mcp",
		// Transport unset → defaults to stdio per parseLifetime.
		Lifetime: tool.LifetimePerSession,
	}, nil, "", nil)
	if err == nil {
		t.Fatal("New should error for stdio spec without Command")
	}
}

// toConfigSpec is a pure field-by-field projection — pin the
// shape so a future refactor can't silently drop a field.
func TestToConfigSpec_FieldByField(t *testing.T) {
	in := tool.Spec{
		Name:      "remote",
		Type:      "mcp",
		Transport: "sse",
		Lifetime:  tool.LifetimePerAgent,
		Command:   "/bin/false",
		Args:      []string{"--flag"},
		Env:       map[string]string{"K": "V"},
		Endpoint:  "https://example/mcp",
		Headers:   map[string]string{"X-Token": "t"},
		Auth:      "hugr",
	}
	got := toConfigSpec(in)
	if got.Name != "remote" || got.Type != "mcp" ||
		got.Transport != "sse" || got.Lifetime != "per_agent" ||
		got.Command != "/bin/false" || got.Endpoint != "https://example/mcp" ||
		got.Auth != "hugr" {
		t.Errorf("scalar fields lost in projection: %+v", got)
	}
	if len(got.Args) != 1 || got.Args[0] != "--flag" {
		t.Errorf("Args: %v", got.Args)
	}
	if got.Env["K"] != "V" {
		t.Errorf("Env: %v", got.Env)
	}
	if got.Headers["X-Token"] != "t" {
		t.Errorf("Headers: %v", got.Headers)
	}
}

// runCleanups skips nil entries and runs the rest in order. Pin
// the contract — Provider.Close() relies on it.
func TestRunCleanups_NilSafe(t *testing.T) {
	var order []int
	runCleanups([]func(){
		func() { order = append(order, 1) },
		nil,
		func() { order = append(order, 2) },
	})
	if len(order) != 2 || order[0] != 1 || order[1] != 2 {
		t.Errorf("order = %v", order)
	}
	// Empty slice is fine.
	runCleanups(nil)
}
