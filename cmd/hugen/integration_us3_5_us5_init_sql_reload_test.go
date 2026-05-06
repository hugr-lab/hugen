// Phase-3.5 US5 init-sql reload regression (T052).
//
// FR-003 / SC-008: operators tune duckdb-mcp's startup hook (memory
// limit, threads, pre-loaded extensions) by editing the `--init-sql`
// arg in tool_providers; a runtime_reload (or a fresh per_session
// spawn) picks up the change. Per_session providers re-read
// cfg.ToolProviders() on every Session.Open, so the simplest correct
// shape is: two separate cores with different --init-sql, two
// sessions, observe two different `current_setting('memory_limit')`
// values.
//
// Skips when uv/uvx not on PATH or vendor/mcp-server-motherduck not
// initialised.
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/config"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session"
	"github.com/hugr-lab/hugen/pkg/skill"
	"github.com/hugr-lab/hugen/pkg/tool"
)

func TestUS3_5_US5_InitSQLReload(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: skipping init-sql reload test")
	}
	for _, bin := range []string{"uv", "uvx"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH; skipping", bin)
		}
	}
	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Skipf("repo root: %v", err)
	}
	vendor := filepath.Join(repoRoot, "vendor", "mcp-server-motherduck")
	if _, err := os.Stat(filepath.Join(vendor, "pyproject.toml")); err != nil {
		t.Skipf("vendor/mcp-server-motherduck not initialised: %v", err)
	}

	queryMemoryLimit := func(t *testing.T, initSQL string) string {
		t.Helper()
		core := newDuckDBCoreWithInitSQL(t, vendor, initSQL)
		ctx := context.Background()
		sess, _, err := core.manager.Open(ctx, session.OpenRequest{OwnerID: "u"})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer core.manager.Terminate(ctx, sess.ID(), "user:/end")

		snap, err := sess.Tools().Snapshot(ctx, sess.ID())
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		exTool, ok := findTool(snap.Tools, "duckdb-mcp:execute_query")
		if !ok {
			t.Fatalf("execute_query missing")
		}
		dispatchCtx := perm.WithSession(ctx, perm.SessionContext{SessionID: sess.ID()})
		args, _ := json.Marshal(map[string]string{
			"sql": "SELECT current_setting('memory_limit') AS m;",
		})
		_, eff, err := sess.Tools().Resolve(dispatchCtx, exTool, args)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		out, err := sess.Tools().Dispatch(dispatchCtx, exTool, eff)
		if err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
		return string(out)
	}

	// DuckDB normalises memory_limit through its byte→IEC formatter.
	// 1GB renders as ~"953.6 MiB" (DuckDB treats 'GB' as 1000^3 and
	// reports IEC); 2GB renders as ~"1.8 GiB". The exact strings
	// differ across DuckDB versions, so match only the integer part
	// of the IEC unit.
	first := queryMemoryLimit(t, "SET memory_limit = '1GB';")
	if !strings.Contains(first, "MiB") {
		t.Errorf("first config did not produce a MiB-shaped memory_limit: %s", first)
	}

	second := queryMemoryLimit(t, "SET memory_limit = '2GB';")
	if !strings.Contains(second, "GiB") {
		t.Errorf("second config did not produce a GiB-shaped memory_limit: %s", second)
	}

	// Defensive: the two responses must differ (catches the case
	// where the initSQL arg was silently ignored).
	if first == second {
		t.Errorf("first and second envelopes identical (init-sql changes ignored?): %s", first)
	}
}

// newDuckDBCoreWithInitSQL spins up a minimal core configured with
// duckdb-mcp + the given --init-sql payload. Mirrors
// newDuckDBIntegrationCore but takes initSQL as a parameter and skips
// the bash-mcp provider (not needed for this regression).
func newDuckDBCoreWithInitSQL(t *testing.T, vendorPath, initSQL string) *integrationCore {
	t.Helper()
	root := t.TempDir()
	workspaceDir := filepath.Join(root, "workspace")
	stateDir := filepath.Join(root, "state")
	for _, d := range []string{workspaceDir, stateDir, filepath.Join(stateDir, "skills/system")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cfgSvc := config.NewStaticService(config.StaticInput{
		ToolProviders: []config.ToolProviderSpec{{
			Name:      "duckdb-mcp",
			Type:      "mcp",
			Transport: "stdio",
			Command:   "uvx",
			Lifetime:  "per_session",
			Args: []string{
				"--from", vendorPath,
				"mcp-server-motherduck",
				"--db-path", ":memory:",
				"--read-write",
				"--init-sql", initSQL,
			},
		}},
	})

	skillStore := skill.NewSkillStore(skill.Options{
		SystemRoot: filepath.Join(stateDir, "skills/system"),
	})
	skills := skill.NewSkillManager(skillStore, nil)
	view := &permsView{rules: nil}
	perms := perm.NewLocalPermissions(view, staticIdentity{id: "agent-it"})
	t.Cleanup(perms.Close)
	tools := tool.NewToolManager(perms, nil, nil)
	t.Cleanup(func() { _ = tools.Close() })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	ws := session.NewWorkspace(workspaceDir, true)
	resources := session.NewResources(session.ResourceDeps{
		Providers:  cfgSvc.ToolProviders(),
		Skills:     skills,
		SkillStore: skillStore,
		Workspace:  ws,
		Logger:     logger,
	})

	router, agent := makeRouter(t)
	mgr := session.NewManager(
		&stubStore{}, agent, router,
		session.NewCommandRegistry(), protocol.NewCodec(), tools, nil,
		session.WithLifecycle(resources),
	)

	return &integrationCore{
		workspaceDir: workspaceDir,
		stateDir:     stateDir,
		tools:        tools,
		skills:       skills,
		skillStore:   skillStore,
		manager:      mgr,
		workspaces:   ws,
	}
}
