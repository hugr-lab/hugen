package runtime

import (
	"testing"

	"github.com/hugr-lab/hugen/pkg/config"
)

func TestIsManagedProvider(t *testing.T) {
	cases := []struct {
		name string
		spec config.ToolProviderSpec
		want bool
	}{
		{"remote http mcp", config.ToolProviderSpec{Name: "x", Transport: "http", Endpoint: "https://h/mcp"}, true},
		{"remote sse mcp", config.ToolProviderSpec{Name: "x", Transport: "sse", Endpoint: "https://h/sse"}, true},
		{"explicit mcp type http", config.ToolProviderSpec{Name: "x", Type: "mcp", Transport: "streamable-http"}, true},
		{"stdio (per_session default)", config.ToolProviderSpec{Name: "bash", Command: "./bash-mcp"}, false},
		{"stdio forced per_agent still not http", config.ToolProviderSpec{Name: "q", Command: "./q", Lifetime: "per_agent"}, false},
		{"http forced per_session", config.ToolProviderSpec{Name: "x", Transport: "http", Lifetime: "per_session"}, false},
		{"non-mcp type", config.ToolProviderSpec{Name: "x", Type: "webhook", Transport: "http"}, false},
	}
	for _, c := range cases {
		if got := isManagedProvider(c.spec); got != c.want {
			t.Errorf("%s: isManagedProvider = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSpecsEqual(t *testing.T) {
	base := config.ToolProviderSpec{
		Name: "x", Transport: "http", Endpoint: "https://h/mcp", Auth: "hugr",
		Headers: map[string]string{"X-Key": "1"},
	}
	same := base
	if !specsEqual(base, same) {
		t.Error("identical specs should be equal")
	}
	diffEndpoint := base
	diffEndpoint.Endpoint = "https://other/mcp"
	if specsEqual(base, diffEndpoint) {
		t.Error("different endpoint should not be equal")
	}
	diffHeader := base
	diffHeader.Headers = map[string]string{"X-Key": "2"}
	if specsEqual(base, diffHeader) {
		t.Error("different headers should not be equal")
	}
}

func TestManagedFrom_FiltersScope(t *testing.T) {
	specs := []config.ToolProviderSpec{
		{Name: "hugr-main", Transport: "http", Endpoint: "https://h/mcp"},    // managed
		{Name: "bash-mcp", Command: "./bash-mcp"},                            // stdio → out
		{Name: "hugr-query", Command: "./hugr-query", Lifetime: "per_agent"}, // stdio per_agent → out
	}
	got := managedFrom(specs)
	if len(got) != 1 {
		t.Fatalf("managedFrom = %d entries, want 1 (%v)", len(got), got)
	}
	if _, ok := got["hugr-main"]; !ok {
		t.Errorf("managedFrom should keep hugr-main, got %v", got)
	}
}
