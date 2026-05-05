// Phase-3.5 smoke test for the in-tree python-mcp server (T042).
//
// Builds bin/python-mcp, runs --create-template against a tiny fixture
// requirements list, then exercises server mode through tool.MCPProvider:
// `run_code("print('hi')")` returns "hi" + exit_code 0. The server lazily
// copies the template into <WORKSPACES_ROOT>/<sid>/.venv/ on the first
// call. Skips when `uv` is not on PATH.
package checks

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/tool"
	mcpprov "github.com/hugr-lab/hugen/pkg/tool/providers/mcp"
)

func TestPythonMCPSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: skipping python-mcp smoke test")
	}
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv not on PATH; skipping python-mcp smoke test")
	}
	root, err := repoRoot()
	if err != nil {
		t.Skipf("repo root: %v", err)
	}

	// Build python-mcp once.
	binDir := t.TempDir()
	bin := filepath.Join(binDir, "python-mcp")
	build := exec.Command("go", "build", "-o", bin, "./mcp/python-mcp")
	build.Dir = root
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build python-mcp: %v", err)
	}

	// Tiny fixture requirements — `six` is the smallest pure-Python
	// transitive-free package; install completes within a few seconds
	// and stresses the venv build path without bringing in the full
	// analyst stack.
	tmp := t.TempDir()
	reqs := filepath.Join(tmp, "requirements.txt")
	if err := os.WriteFile(reqs, []byte("six\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	templateDir := filepath.Join(tmp, "template", ".venv")

	buildVenv := exec.Command(bin, "--create-template", reqs, "--out", templateDir)
	var buildOut bytes.Buffer
	buildVenv.Stdout = &buildOut
	buildVenv.Stderr = &buildOut
	if err := buildVenv.Run(); err != nil {
		t.Fatalf("create-template: %v\n%s", err, buildOut.String())
	}
	if _, err := os.Stat(filepath.Join(templateDir, ".bootstrap-complete")); err != nil {
		t.Fatalf("bootstrap stamp missing: %v", err)
	}

	// Server mode against a fresh workspaces root.
	wsRoot := filepath.Join(tmp, "ws")
	if err := os.MkdirAll(wsRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	prov, err := mcpprov.NewWithSpec(ctx, mcpprov.Spec{
		Name:      "python-mcp",
		Transport: mcpprov.TransportStdio,
		Command:   bin,
		Args:      []string{"--template", templateDir},
		Env: map[string]string{
			"WORKSPACES_ROOT": wsRoot,
			// Hugr env intentionally absent — exercises the US5 path
			// (no Hugr → run_code still works).
		},
		Lifetime:   tool.LifetimePerAgent,
		PermObject: "hugen:tool:python-mcp",
	}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewWithSpec: %v", err)
	}
	t.Cleanup(func() { _ = prov.Close() })

	// Sanity: catalogue exposes both tools.
	tools, err := prov.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := map[string]bool{
		"python-mcp:run_code":   false,
		"python-mcp:run_script": false,
	}
	for _, tl := range tools {
		if _, ok := want[tl.Name]; ok {
			want[tl.Name] = true
		}
	}
	for n, seen := range want {
		if !seen {
			t.Errorf("missing tool %q in catalogue: %v", n, listNames(tools))
		}
	}

	// Drive run_code with a session_id metadata so the server can
	// build per-session venv path.
	sid := "ses-smoke"
	if err := os.MkdirAll(filepath.Join(wsRoot, sid), 0o755); err != nil {
		t.Fatal(err)
	}
	callCtx := perm.WithSession(ctx, perm.SessionContext{SessionID: sid})

	args, _ := json.Marshal(map[string]any{"code": "print('hi')"})
	out, err := prov.Call(callCtx, "run_code", args)
	if err != nil {
		t.Fatalf("run_code: %v", err)
	}
	if !strings.Contains(string(out), "hi") {
		t.Errorf("run_code envelope missing 'hi': %s", string(out))
	}
	// Subsequent call: stamp must already exist (fast path).
	stamp := filepath.Join(wsRoot, sid, ".venv", ".bootstrap-complete")
	if _, err := os.Stat(stamp); err != nil {
		t.Errorf("session venv stamp missing after first call: %v", err)
	}
}

func listNames(tools []tool.Tool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name)
	}
	return out
}
