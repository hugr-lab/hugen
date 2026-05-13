// Phase-3.5 US2 integration test: Python execution surface (T043).
//
// Exercises exit criteria 1, 5, 6, 11 from
// design/001-agent-runtime/phase-3.5-spec.md §10:
//
//   - python.run_code returns stdout in the envelope (criterion 1);
//   - bash.write_file + python.run_script chain produces files on disk
//     (criterion 5);
//   - non-zero exit code surfaces as a normal envelope, not a tool
//     error (criterion 6 / FR-013);
//   - per-session venv lives at <sid>/.venv with .bootstrap-complete,
//     and is reaped along with the workspace on Close (criterion 11).
//
// Skips when `uv` is not on PATH; runs in US5 mode (no Hugr) so the
// auth.Service loopback is not required.
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/config"
	mcpext "github.com/hugr-lab/hugen/pkg/extension/mcp"
	wsext "github.com/hugr-lab/hugen/pkg/extension/workspace"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/runtime"
	"github.com/hugr-lab/hugen/pkg/session"
	"github.com/hugr-lab/hugen/pkg/session/manager"
	"github.com/hugr-lab/hugen/pkg/skill"
	"github.com/hugr-lab/hugen/pkg/tool"
	"github.com/hugr-lab/hugen/pkg/tool/providers"
)

// Cached across subtests in this file: the python-mcp binary built
// once + the relocatable venv template populated against a single
// pure-Python package (`six`). Both are expensive to produce and
// independent of the per-test workspace.
var (
	pythonMCPBinaryOnce sync.Once
	pythonMCPBinary     string
	pythonTemplateOnce  sync.Once
	pythonTemplateDir   string
	pythonTemplateErr   error
)

func buildPythonMCP(t *testing.T) string {
	t.Helper()
	pythonMCPBinaryOnce.Do(func() {
		dir, err := os.MkdirTemp("", "hugen-it-pymcp-*")
		if err != nil {
			t.Fatalf("mktemp: %v", err)
		}
		bin := filepath.Join(dir, "python-mcp")
		cmd := exec.Command("go", "build", "-o", bin, "../../mcp/python-mcp")
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("build python-mcp: %v", err)
		}
		pythonMCPBinary = bin
	})
	return pythonMCPBinary
}

func buildSmokeVenvTemplate(t *testing.T, bin string) string {
	t.Helper()
	pythonTemplateOnce.Do(func() {
		dir, err := os.MkdirTemp("", "hugen-it-pyvenv-*")
		if err != nil {
			pythonTemplateErr = err
			return
		}
		reqs := filepath.Join(dir, "requirements.txt")
		if err := os.WriteFile(reqs, []byte("six\n"), 0o644); err != nil {
			pythonTemplateErr = err
			return
		}
		out := filepath.Join(dir, ".venv")
		cmd := exec.Command(bin, "--create-template", reqs, "--out", out)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			pythonTemplateErr = err
			return
		}
		pythonTemplateDir = out
	})
	if pythonTemplateErr != nil {
		t.Fatalf("build venv template: %v", pythonTemplateErr)
	}
	return pythonTemplateDir
}

func TestAnalyst_Python(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: skipping python-mcp integration test")
	}
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv not on PATH; skipping")
	}

	pyBin := buildPythonMCP(t)
	tmpl := buildSmokeVenvTemplate(t, pyBin)

	core := newPythonIntegrationCore(t, pyBin, tmpl)
	ctx := context.Background()

	if err := core.tools.Init(ctx); err != nil {
		t.Fatalf("ToolManager.Init: %v", err)
	}

	sess, _, err := core.manager.Open(ctx, session.OpenRequest{OwnerID: "u"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = core.manager.Terminate(ctx, sess.ID(), "user:/end") })

	snap, err := sess.Tools().Snapshot(ctx, sess.ID())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	runCode, ok := findTool(snap.Tools, "python-mcp:run_code")
	if !ok {
		t.Fatalf("python-mcp:run_code missing: %v", toolNames(snap.Tools))
	}
	runScript, _ := findTool(snap.Tools, "python-mcp:run_script")
	writeFile, ok := findTool(snap.Tools, "bash-mcp:bash.write_file")
	if !ok {
		t.Fatalf("bash-mcp:bash.write_file missing: %v", toolNames(snap.Tools))
	}

	dispatchCtx := perm.WithSession(ctx, perm.SessionContext{SessionID: sess.ID()})

	dispatch := func(label string, tl tool.Tool, args any) string {
		t.Helper()
		raw, _ := json.Marshal(args)
		_, eff, err := sess.Tools().Resolve(dispatchCtx, tl, raw)
		if err != nil {
			t.Fatalf("%s: Resolve: %v", label, err)
		}
		out, err := sess.Tools().Dispatch(dispatchCtx, tl, eff)
		if err != nil {
			t.Fatalf("%s: Dispatch: %v", label, err)
		}
		return string(out)
	}

	// Criterion 1 — first call pays the venv-bootstrap cost; allow
	// up to 30 s (no CoW: full cp).
	deadline := time.Now().Add(30 * time.Second)
	bootstrap := dispatch("bootstrap", runCode, map[string]any{"code": "print('hi')"})
	if time.Now().After(deadline) {
		t.Errorf("first call took too long (>30s)")
	}
	if !strings.Contains(bootstrap, `"hi`) && !strings.Contains(bootstrap, "hi\\n") {
		t.Errorf("run_code envelope missing 'hi': %s", bootstrap)
	}
	stamp := filepath.Join(core.workspaceDir, sess.ID(), ".venv", ".bootstrap-complete")
	if _, err := os.Stat(stamp); err != nil {
		t.Errorf("bootstrap stamp missing: %v", err)
	}

	// Criterion 5 — write a script then run it.
	dispatch("write script", writeFile, map[string]any{
		"path":    "calc.py",
		"content": "print(6 * 7)\n",
	})
	res := dispatch("run script", runScript, map[string]any{"path": "calc.py"})
	if !strings.Contains(res, "42") {
		t.Errorf("run_script envelope missing 42: %s", res)
	}

	// Criterion 6 — non-zero exit code is a normal envelope, NOT
	// a tool error (FR-013). The agent receives stderr + exit_code.
	exitRaw := dispatch("nonzero exit", runCode, map[string]any{
		"code": "import sys; sys.stderr.write('boom\\n'); sys.exit(2)",
	})
	var exitEnv map[string]any
	if err := json.Unmarshal([]byte(exitRaw), &exitEnv); err == nil {
		// envelope might be wrapped in {"text": "..."} per the
		// MCPProvider text-content marshalling — peek past that.
		if txt, ok := exitEnv["text"].(string); ok {
			exitRaw = txt
		}
	}
	if !strings.Contains(exitRaw, `"exit_code": 2`) &&
		!strings.Contains(exitRaw, `"exit_code":2`) {
		t.Errorf("nonzero exit envelope missing exit_code 2: %s", exitRaw)
	}
	if !strings.Contains(exitRaw, "boom") {
		t.Errorf("nonzero exit envelope missing stderr: %s", exitRaw)
	}

	// Criterion 11 — close the session, assert <sid>/.venv is gone.
	sessDir := filepath.Join(core.workspaceDir, sess.ID())
	if _, err := os.Stat(filepath.Join(sessDir, ".venv")); err != nil {
		t.Fatalf("session venv missing before close: %v", err)
	}
	if err := core.manager.Terminate(ctx, sess.ID(), "user:/end"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(sessDir); !os.IsNotExist(err) {
		t.Errorf("session dir not cleaned: %s", sessDir)
	}
}

// newPythonIntegrationCore wires bash-mcp (per_session) + python-mcp
// (per_agent, US5 path: no auth: hugr) onto a ToolManager. The
// per_agent path needs ToolManager.Init to spawn — caller must invoke
// it explicitly.
func newPythonIntegrationCore(t *testing.T, pyBin, tmpl string) *integrationCore {
	t.Helper()
	root := t.TempDir()
	workspaceDir := filepath.Join(root, "workspace")
	sharedDir := filepath.Join(root, "shared")
	stateDir := filepath.Join(root, "state")
	for _, d := range []string{workspaceDir, sharedDir, stateDir, filepath.Join(stateDir, "skills/hub")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

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
				Name:      "python-mcp",
				Type:      "mcp",
				Transport: "stdio",
				Command:   pyBin,
				Args:      []string{"--template", tmpl},
				Lifetime:  "per_agent",
				// No `auth: hugr` — exercises the US5 path.
			},
		},
	})

	skillStore := skill.NewSkillStore(skill.Options{
		SystemFS: runtime.SystemSkillsFS(),
		HubRoot:  filepath.Join(stateDir, "skills/hub"),
	})
	skills := skill.NewSkillManager(skillStore, nil)
	view := &permsView{rules: nil}
	perms := perm.NewLocalPermissions(view, staticIdentity{id: "agent-it"})
	t.Cleanup(perms.Close)

	// WithWorkspaceRoot is critical here — python-mcp reads
	// WORKSPACES_ROOT to compute <sid>/.venv per call, and the
	// runtime is the only thing that should pin it.
	tools := tool.NewToolManager(perms, cfgSvc.ToolProviders(), nil,
		tool.WithBuilder(providers.NewBuilder(nil, perms, workspaceDir, nil)))
	t.Cleanup(func() { _ = tools.Close() })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	router, agent := makeRouter(t)
	mgr := manager.NewManager(
		&stubStore{}, agent, router,
		session.NewCommandRegistry(), protocol.NewCodec(), tools, nil,
		manager.WithExtensions(
			wsext.NewExtension(workspaceDir, true),
			mcpext.NewExtension(cfgSvc.ToolProviders(), logger),
		),
	)

	return &integrationCore{
		workspaceDir: workspaceDir,
		sharedDir:    sharedDir,
		stateDir:     stateDir,
		tools:        tools,
		skills:       skills,
		skillStore:   skillStore,
		manager:      mgr,
	}
}
