// Phase-3.5 smoke test for the vendored MotherDuck MCP server.
// Spawns `uvx --from ./vendor/mcp-server-motherduck mcp-server-motherduck`
// against a `:memory:` DuckDB and asserts the tool surface that the
// `duckdb-data` skill depends on (T020).
//
// Skips gracefully when `uv` is not on PATH or the submodule is not
// checked out — the same guards CI without uv hits.
package checks

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/tool"
)

func TestDuckDBMCPSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: skipping duckdb-mcp smoke test")
	}
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv not on PATH; skipping duckdb-mcp smoke test")
	}
	if _, err := exec.LookPath("uvx"); err != nil {
		t.Skip("uvx not on PATH; skipping duckdb-mcp smoke test")
	}
	repoRoot, err := repoRoot()
	if err != nil {
		t.Skipf("could not resolve repo root: %v", err)
	}
	vendor := filepath.Join(repoRoot, "vendor", "mcp-server-motherduck")
	if _, err := os.Stat(filepath.Join(vendor, "pyproject.toml")); err != nil {
		t.Skipf("vendor/mcp-server-motherduck not initialised: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	tmp := t.TempDir()
	prov, err := tool.NewMCPProvider(ctx, tool.MCPProviderSpec{
		Name:      "duckdb-mcp",
		Transport: tool.TransportStdio,
		Command:   "uvx",
		Args: []string{
			"--from", vendor,
			"mcp-server-motherduck",
			"--db-path", ":memory:",
			"--read-write",
		},
		Cwd:        tmp,
		Lifetime:   tool.LifetimePerSession,
		PermObject: "hugen:tool:duckdb-mcp",
	}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewMCPProvider: %v", err)
	}
	t.Cleanup(func() { _ = prov.Close() })

	tools, err := prov.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// MCPProvider.List prefixes tool names with the provider name
	// ("duckdb-mcp:execute_query"); MCPProvider.Call takes the bare
	// tool name. Match both shapes here.
	want := map[string]bool{
		"duckdb-mcp:execute_query":  false,
		"duckdb-mcp:list_databases": false,
		"duckdb-mcp:list_tables":    false,
		"duckdb-mcp:list_columns":   false,
	}
	for _, tl := range tools {
		if _, ok := want[tl.Name]; ok {
			want[tl.Name] = true
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("expected tool %q in catalogue; got %v", name, toolNames(tools))
		}
	}

	// Upstream `execute_query(sql)` — the arg key is `sql`, not `query`.
	args, _ := json.Marshal(map[string]string{"sql": "SELECT 42 AS x"})
	out, err := prov.Call(ctx, "execute_query", args)
	if err != nil {
		t.Fatalf("execute_query: %v", err)
	}
	if !strings.Contains(string(out), "42") {
		t.Errorf("execute_query envelope missing '42': %s", string(out))
	}
}

func toolNames(tools []tool.Tool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name)
	}
	return out
}

// repoRoot returns the absolute path of the hugen repository root by
// walking up from the test's working directory until it finds the
// top-level go.mod whose module is github.com/hugr-lab/hugen.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		mod := filepath.Join(dir, "go.mod")
		if data, err := os.ReadFile(mod); err == nil &&
			strings.Contains(string(data), "module github.com/hugr-lab/hugen") {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}
