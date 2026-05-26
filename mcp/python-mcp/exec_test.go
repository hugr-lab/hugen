package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.NewFile(0, os.DevNull), nil))
}

// fakeTemplate creates a minimal "template" dir on disk containing a
// fake bin/python (a shell script that prints its argv) and the
// completion stamp. Returns the absolute path.
func fakeTemplate(t *testing.T) string {
	t.Helper()
	tpl := t.TempDir()
	bin := filepath.Join(tpl, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// `bin/python` is a tiny sh that the production code will exec.
	// It echoes the args so tests can introspect what was invoked.
	script := "#!/bin/sh\necho \"args=$*\"\n"
	if err := os.WriteFile(filepath.Join(bin, "python"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tpl, stampName), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return tpl
}

func TestEnsureSessionVenv_FastPath(t *testing.T) {
	root := t.TempDir()
	tpl := fakeTemplate(t)
	sessDir := filepath.Join(root, "ses-root", "ses-mission")

	// Pre-populate the session venv + stamp so fast path triggers.
	sessVenv := filepath.Join(sessDir, ".venv")
	if err := os.MkdirAll(sessVenv, 0o755); err != nil {
		t.Fatal(err)
	}
	stamp := filepath.Join(sessVenv, stampName)
	if err := os.WriteFile(stamp, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	stat0, _ := os.Stat(stamp)

	deps := &execDeps{template: tpl, log: discardLogger()}
	gotVenv, err := ensureSessionVenv(deps, sessDir)
	if err != nil {
		t.Fatalf("ensureSessionVenv: %v", err)
	}
	if gotVenv != sessVenv {
		t.Errorf("sessVenv = %q want %q", gotVenv, sessVenv)
	}
	stat1, _ := os.Stat(stamp)
	if !stat0.ModTime().Equal(stat1.ModTime()) {
		t.Errorf("fast path rewrote stamp; modtime changed")
	}
}

func TestEnsureSessionVenv_Bootstrap(t *testing.T) {
	root := t.TempDir()
	tpl := fakeTemplate(t)
	sessDir := filepath.Join(root, "ses-root", "ses-mission-new")
	deps := &execDeps{template: tpl, log: discardLogger()}

	sessVenv, err := ensureSessionVenv(deps, sessDir)
	if err != nil {
		t.Fatalf("ensureSessionVenv: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sessVenv, stampName)); err != nil {
		t.Errorf("stamp not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sessVenv, "bin", "python")); err != nil {
		t.Errorf("python not copied: %v", err)
	}
}

func TestEnsureSessionVenv_PartialRecovery(t *testing.T) {
	root := t.TempDir()
	tpl := fakeTemplate(t)
	sessDir := filepath.Join(root, "ses-root", "ses-broken")

	// Pre-populate a partial venv (no stamp file).
	sessVenv := filepath.Join(sessDir, ".venv")
	if err := os.MkdirAll(sessVenv, 0o755); err != nil {
		t.Fatal(err)
	}
	junk := filepath.Join(sessVenv, "junk")
	if err := os.WriteFile(junk, []byte("from prior crash"), 0o644); err != nil {
		t.Fatal(err)
	}

	deps := &execDeps{template: tpl, log: discardLogger()}
	if _, err := ensureSessionVenv(deps, sessDir); err != nil {
		t.Fatalf("ensureSessionVenv: %v", err)
	}
	if _, err := os.Stat(junk); !os.IsNotExist(err) {
		t.Errorf("junk file survived re-bootstrap; err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(sessVenv, stampName)); err != nil {
		t.Errorf("new stamp missing: %v", err)
	}
}

func TestEnsureSessionVenv_TemplateMissing(t *testing.T) {
	deps := &execDeps{
		template: "/nonexistent/template",
		log:      discardLogger(),
	}
	_, err := ensureSessionVenv(deps, filepath.Join(t.TempDir(), "ses-x"))
	if err == nil {
		t.Fatalf("expected error when template is missing")
	}
	if !strings.Contains(err.Error(), "template missing") {
		t.Errorf("err=%q does not mention template", err)
	}
}

// TestEnsureSessionVenv_ConcurrentBootstrap exercises the per-dir
// mutex: two goroutines bootstrap the SAME session_dir at once (the
// 5.4 mission-shared layout case). Both must succeed, the .venv
// must be fully populated, and only one bootstrap pass should
// actually run copyTree (verified by checking the stamp mtime
// doesn't get rewritten by the second caller).
func TestEnsureSessionVenv_ConcurrentBootstrap(t *testing.T) {
	root := t.TempDir()
	tpl := fakeTemplate(t)
	sessDir := filepath.Join(root, "ses-root", "ses-mission-shared")
	deps := &execDeps{template: tpl, log: discardLogger()}

	const workers = 4
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		go func() {
			_, err := ensureSessionVenv(deps, sessDir)
			errs <- err
		}()
	}
	for i := 0; i < workers; i++ {
		if err := <-errs; err != nil {
			t.Errorf("worker %d: %v", i, err)
		}
	}
	if _, err := os.Stat(filepath.Join(sessDir, ".venv", stampName)); err != nil {
		t.Errorf("stamp missing after concurrent bootstrap: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sessDir, ".venv", "bin", "python")); err != nil {
		t.Errorf("python missing after concurrent bootstrap: %v", err)
	}
}

func TestResolveScriptPath(t *testing.T) {
	sess := t.TempDir()
	good := filepath.Join(sess, "ok.py")
	if err := os.WriteFile(good, []byte("print(1)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, in string
		wantCode string // "" = success
	}{
		{"relative ok", "ok.py", ""},
		{"absolute rejected", filepath.Join(sess, "ok.py"), "arg_validation"},
		{"escape rejected", "../etc/passwd", "arg_validation"},
		{"missing", "missing.py", "not_found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, terr := resolveScriptPath(sess, c.in)
			if c.wantCode == "" {
				if terr != nil {
					t.Fatalf("unexpected err: %v", terr)
				}
				return
			}
			if terr == nil || terr.Code != c.wantCode {
				t.Fatalf("got %v, want code=%s", terr, c.wantCode)
			}
		})
	}
}

func TestComposeChildEnv(t *testing.T) {
	t.Setenv("HUGR_ACCESS_TOKEN", "go-side-secret")
	t.Setenv("HUGR_TOKEN_URL", "http://loopback")
	t.Setenv("HUGR_TOKEN", "stale-from-parent")
	t.Setenv("HUGR_URL", "http://stale-parent")
	t.Setenv("MY_OWN_VAR", "kept")

	env := composeChildEnv("http://hub", "fresh-jwt", "/sessions/s1")

	expectAbsent := []string{"HUGR_ACCESS_TOKEN", "HUGR_TOKEN_URL"}
	for _, key := range expectAbsent {
		for _, kv := range env {
			if strings.HasPrefix(kv, key+"=") {
				t.Errorf("env should not carry %s, got %q", key, kv)
			}
		}
	}

	expectExact := map[string]string{
		"HUGR_URL":                "http://hub",
		"HUGR_TOKEN":              "fresh-jwt",
		"PYTHONUNBUFFERED":        "1",
		"PYTHONDONTWRITEBYTECODE": "1",
		"MPLBACKEND":              "Agg",
		"SESSION_DIR":             "/sessions/s1",
		"MY_OWN_VAR":              "kept",
	}
	for k, want := range expectExact {
		got := envValue(env, k)
		if got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestComposeChildEnv_NoHugr(t *testing.T) {
	t.Setenv("HUGR_TOKEN", "stale")
	t.Setenv("HUGR_URL", "stale")
	env := composeChildEnv("", "", "/d")
	if v := envValue(env, "HUGR_TOKEN"); v != "" {
		t.Errorf("HUGR_TOKEN should be absent in no-Hugr path, got %q", v)
	}
	if v := envValue(env, "HUGR_URL"); v != "" {
		t.Errorf("HUGR_URL should be absent in no-Hugr path, got %q", v)
	}
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return strings.TrimPrefix(kv, prefix)
		}
	}
	return ""
}

func TestCappedBuffer(t *testing.T) {
	b := &cappedBuffer{cap: 8}
	if _, err := b.Write([]byte("123")); err != nil {
		t.Fatal(err)
	}
	if b.truncated {
		t.Errorf("truncated too early")
	}
	if _, err := b.Write([]byte("4567890")); err != nil {
		t.Fatal(err)
	}
	if !b.truncated {
		t.Errorf("expected truncated=true after overflow")
	}
	got := b.String()
	if !strings.HasPrefix(got, "12345678") {
		t.Errorf("kept bytes lost: %q", got)
	}
	if !strings.Contains(got, "[output truncated]") {
		t.Errorf("missing truncation marker: %q", got)
	}
}

func TestSessionDirFromRequest(t *testing.T) {
	var req mcp.CallToolRequest
	if got := sessionDirFromRequest(req); got != "" {
		t.Errorf("nil meta should yield empty, got %q", got)
	}
	req.Params.Meta = &mcp.Meta{AdditionalFields: map[string]any{"session_dir": "/work/ses-r/ses-m"}}
	if got := sessionDirFromRequest(req); got != "/work/ses-r/ses-m" {
		t.Errorf("got %q want /work/ses-r/ses-m", got)
	}
	req.Params.Meta = &mcp.Meta{AdditionalFields: map[string]any{"session_dir": 42}}
	if got := sessionDirFromRequest(req); got != "" {
		t.Errorf("non-string value should yield empty, got %q", got)
	}
}

func TestFlattenKwargs(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]string
		want []string
	}{
		{"nil", nil, nil},
		{"empty", map[string]string{}, nil},
		{
			"sorted",
			map[string]string{"beta": "2", "alpha": "1", "gamma": "3"},
			[]string{"--alpha", "1", "--beta", "2", "--gamma", "3"},
		},
		{
			"single",
			map[string]string{"only": "v"},
			[]string{"--only", "v"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := flattenKwargs(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("flattenKwargs(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// buildScriptRequest constructs an mcp.CallToolRequest with the
// run_script-shaped argument bag. mcp-go's GetArguments() expects
// Arguments to already be a map[string]any (what the server-side
// JSON decoder hands back); tests set it directly.
func buildScriptRequest(t *testing.T, args map[string]any) mcp.CallToolRequest {
	t.Helper()
	var req mcp.CallToolRequest
	req.Params.Arguments = args
	return req
}

func TestParseRunArgs_Kwargs(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		req := buildScriptRequest(t, map[string]any{
			"path":   "ok.py",
			"args":   []any{"first", "second"},
			"kwargs": map[string]any{"material_id": "m-7", "format": "html"},
		})
		r := runRequest{kind: "run_script"}
		if err := parseRunArgs(req, &r); err != nil {
			t.Fatalf("parseRunArgs: %v", err)
		}
		if r.path != "ok.py" {
			t.Errorf("path = %q", r.path)
		}
		if !reflect.DeepEqual(r.scriptArg, []string{"first", "second"}) {
			t.Errorf("scriptArg = %v", r.scriptArg)
		}
		want := map[string]string{"material_id": "m-7", "format": "html"}
		if !reflect.DeepEqual(r.scriptKwarg, want) {
			t.Errorf("scriptKwarg = %v want %v", r.scriptKwarg, want)
		}
	})

	t.Run("non-string value rejected", func(t *testing.T) {
		req := buildScriptRequest(t, map[string]any{
			"path":   "ok.py",
			"kwargs": map[string]any{"count": 42},
		})
		r := runRequest{kind: "run_script"}
		err := parseRunArgs(req, &r)
		if err == nil {
			t.Fatalf("expected arg_validation error")
		}
		te, ok := err.(*toolError)
		if !ok || te.Code != "arg_validation" {
			t.Fatalf("got %v, want arg_validation toolError", err)
		}
		if !strings.Contains(te.Msg, "count") {
			t.Errorf("err should name the offending key, got %q", te.Msg)
		}
	})

	// Each of these would break argv parsing downstream — an empty
	// key becomes "--" (POSIX end-of-options sentinel), "=" collapses
	// key+value, leading "-" produces "---x", whitespace/metachars
	// confuse argparse-style consumers.
	badKeys := []string{"", "-leading-dash", "has=equals", "with space", "semi;colon", "1starts-digit"}
	for _, k := range badKeys {
		t.Run("bad-key/"+k, func(t *testing.T) {
			req := buildScriptRequest(t, map[string]any{
				"path":   "ok.py",
				"kwargs": map[string]any{k: "v"},
			})
			r := runRequest{kind: "run_script"}
			err := parseRunArgs(req, &r)
			te, ok := err.(*toolError)
			if !ok || te.Code != "arg_validation" {
				t.Fatalf("expected arg_validation rejection for key %q, got %v", k, err)
			}
		})
	}

	t.Run("dash-in-key allowed", func(t *testing.T) {
		req := buildScriptRequest(t, map[string]any{
			"path":   "ok.py",
			"kwargs": map[string]any{"my-flag": "v"},
		})
		r := runRequest{kind: "run_script"}
		if err := parseRunArgs(req, &r); err != nil {
			t.Fatalf("dash should be allowed mid-key: %v", err)
		}
	})

	t.Run("absent kwargs leaves nil map", func(t *testing.T) {
		req := buildScriptRequest(t, map[string]any{"path": "ok.py"})
		r := runRequest{kind: "run_script"}
		if err := parseRunArgs(req, &r); err != nil {
			t.Fatalf("parseRunArgs: %v", err)
		}
		if r.scriptKwarg != nil {
			t.Errorf("scriptKwarg should be nil, got %v", r.scriptKwarg)
		}
	})
}

// TestRunScript_KwargsArgvOrder is an end-to-end check that
// run_script lays out argv as <script> <positional...> <--key val
// sorted...>. Uses the fake template's bin/python (a sh script that
// echoes its argv), so stdout reveals the exact argv ordering.
func TestRunScript_KwargsArgvOrder(t *testing.T) {
	root := t.TempDir()
	tpl := fakeTemplate(t)
	sessDir := filepath.Join(root, "ses-root", "ses-mission")
	deps := &execDeps{template: tpl, log: discardLogger()}
	sessVenv, err := ensureSessionVenv(deps, sessDir)
	if err != nil {
		t.Fatalf("ensureSessionVenv: %v", err)
	}
	scriptPath := filepath.Join(sessDir, "report.py")
	if err := os.WriteFile(scriptPath, []byte("# fake\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := runRequest{
		kind:        "run_script",
		path:        "report.py",
		scriptArg:   []string{"positional1", "positional2"},
		scriptKwarg: map[string]string{"beta": "2", "alpha": "1"},
		timeoutMs:   5000,
	}
	res, terr := runPython(context.Background(), deps, r, sessDir, sessVenv, "", "")
	if terr != nil {
		t.Fatalf("runPython: %v", terr)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit_code=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	// fakeTemplate's bin/python is `echo "args=$*"`. The full argv
	// after the shebang is: <scriptPath> positional1 positional2
	// --alpha 1 --beta 2. We can match on suffix to avoid hardcoding
	// the abs path prefix.
	want := "positional1 positional2 --alpha 1 --beta 2"
	if !strings.Contains(res.Stdout, want) {
		t.Errorf("stdout missing argv tail %q\ngot: %q", want, res.Stdout)
	}
	// Sanity: kwargs sort BEFORE the positional args would be a bug.
	idxPos := strings.Index(res.Stdout, "positional1")
	idxKw := strings.Index(res.Stdout, "--alpha")
	if idxPos < 0 || idxKw < 0 || idxPos > idxKw {
		t.Errorf("kwargs must come after positional argv; stdout=%q", res.Stdout)
	}
}

// silence unused-import if context becomes a no-go at any future
// refactor.
var _ = context.Background
