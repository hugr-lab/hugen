// Phase-3.5 US1 integration test: DuckDB analytical SQL.
//
// Exercises exit criteria 2, 3, 4 from
// design/001-agent-runtime/phase-3.5-spec.md §10:
//
//   - read a Parquet file via read_parquet(...) — schema + row count
//     come back inline (criterion 2);
//   - convert that file to CSV via COPY ... TO 'data/out.csv'
//     (criterion 3);
//   - run a spatial query without any per-call INSTALL — proves the
//     operator's --init-sql preloaded `spatial` (criterion 4);
//   - assert the per-session DuckDB scratch (`<sid>/.duckdb/`) is
//     gone after Close (criterion 4 cleanup).
//
// Skips when `uv`/`uvx` is not on PATH or vendor/mcp-server-motherduck
// is not initialised — same shape as the duckdb-mcp smoke test.
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

func TestUS3_5_US1_DuckDBSQL(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: skipping duckdb-mcp integration test")
	}
	for _, bin := range []string{"uv", "uvx"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH; skipping", bin)
		}
	}
	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Skipf("could not resolve repo root: %v", err)
	}
	vendor := filepath.Join(repoRoot, "vendor", "mcp-server-motherduck")
	if _, err := os.Stat(filepath.Join(vendor, "pyproject.toml")); err != nil {
		t.Skipf("vendor/mcp-server-motherduck not initialised: %v", err)
	}

	core := newDuckDBIntegrationCore(t, vendor)

	ctx := context.Background()
	sess, _, err := core.manager.Open(ctx, session.OpenRequest{OwnerID: "u"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = core.manager.Terminate(ctx, sess.ID(), "user:/end") })

	snap, err := core.tools.Snapshot(ctx, sess.ID())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	exec, ok := findTool(snap.Tools, "duckdb-mcp:execute_query")
	if !ok {
		t.Fatalf("duckdb-mcp:execute_query missing from snapshot: %v", toolNames(snap.Tools))
	}

	dispatchCtx := perm.WithSession(ctx, perm.SessionContext{SessionID: sess.ID()})

	// Helper: run one execute_query against the per-session connection.
	run := func(label, sql string) string {
		t.Helper()
		args, _ := json.Marshal(map[string]string{"sql": sql})
		_, eff, err := core.tools.Resolve(dispatchCtx, exec, args)
		if err != nil {
			t.Fatalf("%s: Resolve: %v", label, err)
		}
		out, err := core.tools.Dispatch(dispatchCtx, exec, eff)
		if err != nil {
			t.Fatalf("%s: Dispatch: %v", label, err)
		}
		return string(out)
	}

	// Criterion 2 — produce a Parquet file in the session workspace
	// (so the test is self-contained without a fixture file shipped
	// with the repo) and read its row count + schema.
	run("seed parquet",
		`COPY (SELECT * FROM (VALUES `+
			`(1, 'alpha'), (2, 'beta'), (3, 'gamma')) `+
			`AS t(id, name)) TO 'customers.parquet';`)

	// Schema introspection inline.
	schema := run("schema",
		`DESCRIBE FROM read_parquet('customers.parquet');`)
	for _, want := range []string{"id", "name"} {
		if !strings.Contains(schema, want) {
			t.Errorf("schema envelope missing column %q: %s", want, schema)
		}
	}
	count := run("row count",
		`SELECT count() AS n FROM read_parquet('customers.parquet');`)
	if !strings.Contains(count, "3") {
		t.Errorf("row count envelope missing 3: %s", count)
	}

	// Criterion 3 — convert Parquet → CSV via COPY.
	run("convert to csv",
		`COPY (SELECT * FROM read_parquet('customers.parquet')) `+
			`TO 'customers.csv' (FORMAT csv, HEADER);`)
	csvPath := filepath.Join(core.workspaceDir, sess.ID(), "customers.csv")
	body, err := os.ReadFile(csvPath)
	if err != nil {
		t.Fatalf("read converted csv: %v", err)
	}
	for _, want := range []string{"id,name", "alpha", "beta", "gamma"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("csv missing %q: %s", want, body)
		}
	}

	// Criterion 4 — spatial query without per-call INSTALL. Confirms
	// the operator's --init-sql preloaded `spatial`.
	spatial := run("spatial",
		`SET geometry_always_xy = true; `+
			`SELECT ST_Distance_Spheroid(`+
			`ST_Point(13.4050, 52.5200)::POINT_2D, `+
			`ST_Point(2.3522, 48.8566)::POINT_2D) AS m;`)
	// Berlin → Paris ≈ 878 km. Just check we got a non-trivial number.
	if !strings.Contains(spatial, "8") || strings.Contains(spatial, "Catalog Error") {
		t.Errorf("spatial query did not return a distance: %s", spatial)
	}

	// Criterion 4 cleanup — closing the session removes <sid>/ entirely
	// (workspace.Release with cleanup=true), including .duckdb/.
	sessDir := filepath.Join(core.workspaceDir, sess.ID())
	duckScratch := filepath.Join(sessDir, ".duckdb")
	// .duckdb may or may not exist (DuckDB creates tmp/secrets lazily);
	// existence isn't required, only "gone after close".
	if core.manager.Terminate(ctx, sess.ID(), "user:/end"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(sessDir); !os.IsNotExist(err) {
		t.Errorf("session dir %s not cleaned", sessDir)
	}
	if _, err := os.Stat(duckScratch); !os.IsNotExist(err) {
		t.Errorf("duckdb scratch %s not cleaned", duckScratch)
	}
}

// newDuckDBIntegrationCore is a test harness mirroring newIntegrationCore
// but configures a duckdb-mcp provider alongside bash-mcp. The
// duckdb-mcp args mirror config.example.yaml — vendored MotherDuck MCP
// via uvx, in-memory db, hardening + extension preload via --init-sql.
func newDuckDBIntegrationCore(t *testing.T, vendorPath string) *integrationCore {
	t.Helper()
	root := t.TempDir()
	workspaceDir := filepath.Join(root, "workspace")
	sharedDir := filepath.Join(root, "shared")
	stateDir := filepath.Join(root, "state")
	for _, d := range []string{workspaceDir, sharedDir, stateDir, filepath.Join(stateDir, "skills/system")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	initSQL := strings.Join([]string{
		"SET memory_limit = '1GB';",
		"SET threads = 2;",
		"SET temp_directory = '.duckdb/tmp';",
		"SET secret_directory = '.duckdb/secrets';",
		"INSTALL spatial; LOAD spatial;",
		"INSTALL httpfs;  LOAD httpfs;",
	}, "\n")

	cfgSvc := config.NewStaticService(config.StaticInput{
		ToolProviders: []config.ToolProviderSpec{
			{
				Name:      "bash-mcp",
				Type:      "mcp",
				Transport: "stdio",
				Command:   bashMCPBinary,
				Lifetime:  "per_session",
				Env:       map[string]string{"SHARED_DIR": sharedDir},
			},
			{
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
					"--allow-switch-databases",
					"--init-sql", initSQL,
				},
			},
		},
	})

	skillStore := skill.NewSkillStore(skill.Options{
		SystemRoot: filepath.Join(stateDir, "skills/system"),
	})
	skills := skill.NewSkillManager(skillStore, nil)
	view := &permsView{rules: nil}
	perms := perm.NewLocalPermissions(view, staticIdentity{id: "agent-it"})
	t.Cleanup(perms.Close)
	tools := tool.NewToolManager(perms, nil, nil, nil)
	t.Cleanup(func() { _ = tools.Close() })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	ws := session.NewWorkspace(workspaceDir, true)
	resources := session.NewResources(session.ResourceDeps{
		Providers:  cfgSvc.ToolProviders(),
		Tools:      tools,
		Skills:     skills,
		SkillStore: skillStore,
		Workspace:  ws,
		Logger:     logger,
	})

	router, agent := makeRouter(t)
	mgr := session.NewManager(
		&stubStore{}, agent, router,
		session.NewCommandRegistry(), protocol.NewCodec(), nil,
		session.WithLifecycle(resources),
		session.WithSessionOptions(session.WithTools(tools)),
	)

	return &integrationCore{
		workspaceDir: workspaceDir,
		sharedDir:    sharedDir,
		stateDir:     stateDir,
		tools:        tools,
		skills:       skills,
		skillStore:   skillStore,
		manager:      mgr,
		workspaces:   ws,
	}
}

// findRepoRoot mirrors internal/checks/duckdb_mcp_smoke_test.go:repoRoot.
// Walks up from the test cwd until it finds the hugen go.mod.
func findRepoRoot() (string, error) {
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
