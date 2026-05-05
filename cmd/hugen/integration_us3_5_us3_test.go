// Phase-3.5 US3 integration test: end-to-end analyst loop (T044).
//
// Hugr → Parquet → DuckDB → Python → HTML+PDF in a single session.
// Exit criterion 8 from design/001-agent-runtime/phase-3.5-spec.md §10.
//
// Gated by HUGEN_FULL_E2E=1 — the test requires the full analyst venv
// (pandas, pyarrow, plotly, weasyprint), which in turn pulls system
// libraries (Cairo, Pango, gdk-pixbuf, libffi) the operator must
// install once. The test seeds the Parquet output via DuckDB COPY in
// place of an actual hugr-query call (the Hugr leg is exercised
// separately by hugr-data integration tests when HUGR_URL is set).
//
// Skips cleanly when HUGEN_FULL_E2E unset, uv not on PATH, vendored
// MCP not initialised, or HUGEN_PYTHON_TEMPLATE missing / incomplete.
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
	"github.com/hugr-lab/hugen/pkg/tool/providers"
)

func TestUS3_5_US3_AnalystLoop(t *testing.T) {
	if os.Getenv("HUGEN_FULL_E2E") != "1" {
		t.Skip("HUGEN_FULL_E2E != 1; skipping full analyst-loop e2e")
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
	tmpl := os.Getenv("HUGEN_PYTHON_TEMPLATE")
	if tmpl == "" {
		t.Skip("HUGEN_PYTHON_TEMPLATE unset; build the analyst venv first")
	}
	if _, err := os.Stat(filepath.Join(tmpl, ".bootstrap-complete")); err != nil {
		t.Skipf("python template not ready (%s): %v", tmpl, err)
	}
	pyBin := buildPythonMCP(t)

	core := newAnalystIntegrationCore(t, pyBin, tmpl, vendor)
	ctx := context.Background()
	if err := core.tools.Init(ctx); err != nil {
		t.Fatalf("ToolManager.Init: %v", err)
	}

	sess, _, err := core.manager.Open(ctx, session.OpenRequest{OwnerID: "u"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = core.manager.Terminate(ctx, sess.ID(), "user:/end") })

	snap, err := core.tools.Snapshot(ctx, sess.ID())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	mustTool := func(name string) tool.Tool {
		t.Helper()
		tl, ok := findTool(snap.Tools, name)
		if !ok {
			t.Fatalf("%s missing in snapshot", name)
		}
		return tl
	}
	execTool := mustTool("duckdb-mcp:execute_query")
	runScript := mustTool("python-mcp:run_script")
	writeFile := mustTool("bash-mcp:bash.write_file")

	dispatchCtx := perm.WithSession(ctx, perm.SessionContext{SessionID: sess.ID()})
	dispatch := func(label string, tl tool.Tool, args any) string {
		t.Helper()
		raw, _ := json.Marshal(args)
		_, eff, err := core.tools.Resolve(dispatchCtx, tl, raw)
		if err != nil {
			t.Fatalf("%s: Resolve: %v", label, err)
		}
		out, err := core.tools.Dispatch(dispatchCtx, tl, eff)
		if err != nil {
			t.Fatalf("%s: Dispatch: %v", label, err)
		}
		return string(out)
	}

	// Leg 1 — seed the Parquet output (stand-in for hugr-query).
	dispatch("seed parquet", execTool, map[string]string{
		"sql": "COPY (SELECT * FROM (VALUES " +
			"('north', 100), ('south', 250), ('east', 175), ('west', 90)) " +
			"AS t(region, sales)) TO 'sales.parquet';",
	})

	// Leg 2 — DuckDB aggregate.
	agg := dispatch("aggregate", execTool, map[string]string{
		"sql": "SELECT region, sum(sales) AS total " +
			"FROM read_parquet('sales.parquet') GROUP BY 1 ORDER BY total DESC;",
	})
	if !strings.Contains(agg, "south") {
		t.Errorf("aggregate envelope missing region: %s", agg)
	}

	// Leg 3 — Python: pandas read + plotly bar chart → self-contained HTML.
	dispatch("write plot script", writeFile, map[string]any{
		"path": "plot.py",
		"content": `
import os
import pandas as pd
import plotly.express as px

os.makedirs("reports", exist_ok=True)
df = pd.read_parquet("sales.parquet")
fig = px.bar(df, x="region", y="sales", title="Sales by region")
fig.write_html("reports/sales.html", include_plotlyjs="inline", full_html=True)
print("wrote", "reports/sales.html")
`,
	})
	dispatch("run plot", runScript, map[string]string{"path": "plot.py"})
	htmlPath := filepath.Join(core.workspaceDir, sess.ID(), "reports", "sales.html")
	body, err := os.ReadFile(htmlPath)
	if err != nil {
		t.Fatalf("read html: %v", err)
	}
	if !strings.Contains(string(body), "plotly.min.js") &&
		!strings.Contains(string(body), "Plotly.newPlot") {
		t.Errorf("HTML report does not look self-contained: %d bytes", len(body))
	}

	// Leg 4 — weasyprint HTML → PDF.
	dispatch("write pdf script", writeFile, map[string]any{
		"path": "topdf.py",
		"content": `
import os
from weasyprint import HTML

os.makedirs("reports", exist_ok=True)
HTML(filename="reports/sales.html").write_pdf("reports/sales.pdf")
print("ok")
`,
	})
	dispatch("run pdf", runScript, map[string]string{"path": "topdf.py"})
	pdfPath := filepath.Join(core.workspaceDir, sess.ID(), "reports", "sales.pdf")
	pdf, err := os.ReadFile(pdfPath)
	if err != nil {
		t.Fatalf("read pdf: %v", err)
	}
	if !strings.HasPrefix(string(pdf), "%PDF-") {
		t.Errorf("not a PDF (magic header): %q", pdf[:8])
	}
}

// newAnalystIntegrationCore wires bash-mcp (per_session) +
// duckdb-mcp (per_session) + python-mcp (per_agent, US5 mode) onto a
// single ToolManager.
func newAnalystIntegrationCore(t *testing.T, pyBin, tmpl, vendor string) *integrationCore {
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
					"--from", vendor,
					"mcp-server-motherduck",
					"--db-path", ":memory:",
					"--read-write",
					"--allow-switch-databases",
					"--init-sql", initSQL,
				},
			},
			{
				Name:      "python-mcp",
				Type:      "mcp",
				Transport: "stdio",
				Command:   pyBin,
				Args:      []string{"--template", tmpl},
				Lifetime:  "per_agent",
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
	tools := tool.NewToolManager(perms, cfgSvc.ToolProviders(), nil,
		tool.WithBuilder(providers.NewBuilder(nil, perms, workspaceDir, nil)))
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
