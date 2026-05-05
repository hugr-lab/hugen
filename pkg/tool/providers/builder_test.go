package providers

import (
	"context"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/tool"
)

func TestBuilder_DispatchesUnknownType(t *testing.T) {
	b := NewBuilder(nil, nil, "", nil)
	_, err := b.Build(context.Background(), tool.Spec{Name: "x", Type: "webhook"})
	if err == nil {
		t.Fatal("Build should error for unknown type")
	}
	if !strings.Contains(err.Error(), "unknown type") {
		t.Errorf("err = %v", err)
	}
}

// Empty / "mcp" type both route to the mcp subpackage. We don't
// stand up a real MCP transport here — verifying the dispatch
// reaches mcp.New is enough; mcp's own tests pin the construction
// behaviour. Both shapes fail at the same validation point (stdio
// without Command), which is what we assert.
func TestBuilder_RoutesMCPDefault(t *testing.T) {
	b := NewBuilder(nil, nil, "", nil)
	for _, typ := range []string{"", "mcp", "MCP"} {
		_, err := b.Build(context.Background(), tool.Spec{
			Name:     "broken",
			Type:     typ,
			Lifetime: tool.LifetimePerSession,
		})
		if err == nil {
			t.Errorf("type=%q: expected error from mcp.New (stdio without Command)", typ)
		}
		// The error is wrapped from BuildMCPProviderSpec — must
		// NOT mention "unknown type" (that's the default-case path).
		if err != nil && strings.Contains(err.Error(), "unknown type") {
			t.Errorf("type=%q: routed to default case instead of mcp: %v", typ, err)
		}
	}
}
