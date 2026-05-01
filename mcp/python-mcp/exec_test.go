package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
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

	// Pre-populate the session venv + stamp so fast path triggers.
	sessVenv := filepath.Join(root, "ses-1", ".venv")
	if err := os.MkdirAll(sessVenv, 0o755); err != nil {
		t.Fatal(err)
	}
	stamp := filepath.Join(sessVenv, stampName)
	if err := os.WriteFile(stamp, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	stat0, _ := os.Stat(stamp)

	deps := &execDeps{template: tpl, workspacesRoot: root, log: discardLogger()}
	sessDir, gotVenv, err := ensureSessionVenv(deps, "ses-1")
	if err != nil {
		t.Fatalf("ensureSessionVenv: %v", err)
	}
	if sessDir != filepath.Join(root, "ses-1") {
		t.Errorf("sessDir = %q", sessDir)
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
	deps := &execDeps{template: tpl, workspacesRoot: root, log: discardLogger()}

	sessDir, sessVenv, err := ensureSessionVenv(deps, "ses-new")
	if err != nil {
		t.Fatalf("ensureSessionVenv: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sessVenv, stampName)); err != nil {
		t.Errorf("stamp not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sessVenv, "bin", "python")); err != nil {
		t.Errorf("python not copied: %v", err)
	}
	if sessDir != filepath.Join(root, "ses-new") {
		t.Errorf("sessDir = %q", sessDir)
	}
}

func TestEnsureSessionVenv_PartialRecovery(t *testing.T) {
	root := t.TempDir()
	tpl := fakeTemplate(t)

	// Pre-populate a partial venv (no stamp file).
	sessVenv := filepath.Join(root, "ses-broken", ".venv")
	if err := os.MkdirAll(sessVenv, 0o755); err != nil {
		t.Fatal(err)
	}
	junk := filepath.Join(sessVenv, "junk")
	if err := os.WriteFile(junk, []byte("from prior crash"), 0o644); err != nil {
		t.Fatal(err)
	}

	deps := &execDeps{template: tpl, workspacesRoot: root, log: discardLogger()}
	if _, _, err := ensureSessionVenv(deps, "ses-broken"); err != nil {
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
		template:       "/nonexistent/template",
		workspacesRoot: t.TempDir(),
		log:            discardLogger(),
	}
	_, _, err := ensureSessionVenv(deps, "ses-x")
	if err == nil {
		t.Fatalf("expected error when template is missing")
	}
	if !strings.Contains(err.Error(), "template missing") {
		t.Errorf("err=%q does not mention template", err)
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

func TestSessionIDFromRequest(t *testing.T) {
	var req mcp.CallToolRequest
	if got := sessionIDFromRequest(req); got != "" {
		t.Errorf("nil meta should yield empty, got %q", got)
	}
	req.Params.Meta = &mcp.Meta{AdditionalFields: map[string]any{"session_id": "ses-7"}}
	if got := sessionIDFromRequest(req); got != "ses-7" {
		t.Errorf("got %q want ses-7", got)
	}
	req.Params.Meta = &mcp.Meta{AdditionalFields: map[string]any{"session_id": 42}}
	if got := sessionIDFromRequest(req); got != "" {
		t.Errorf("non-string id should yield empty, got %q", got)
	}
}

// silence unused-import if context becomes a no-go at any future
// refactor.
var _ = context.Background
