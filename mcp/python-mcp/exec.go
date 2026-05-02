package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// execDeps groups the per-server immutable dependencies the tool
// handlers consult on every call. Constructed once in main, passed
// to registerTools.
type execDeps struct {
	template       string // absolute path to the relocatable template venv
	workspacesRoot string // absolute path of <state>/workspaces (parent of <sid>/)
	auth           *authSource
	log            *slog.Logger
}

// Tool result envelope. Shared between run_code and run_script.
type runResult struct {
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	ExitCode  int    `json:"exit_code"`
	ElapsedMs int64  `json:"elapsed_ms"`
	Truncated bool   `json:"output_truncated,omitempty"`
}

// toolError matches the envelope hugr-query uses so the agent's
// MCPProvider deserialises a consistent shape.
type toolError struct {
	Code string `json:"code"`
	Msg  string `json:"message"`
}

func (e *toolError) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Msg) }

const (
	defaultTimeoutMs int64 = 5 * 60 * 1000      // 5 min
	maxTimeoutMs     int64 = 2 * 60 * 60 * 1000 // 2 h
	maxOutputBytes         = 32 * 1024
)

func registerTools(srv *server.MCPServer, deps *execDeps) {
	srv.AddTool(mcp.NewTool("run_code",
		mcp.WithDescription(`Execute Python source in the per-session venv. Returns {stdout, stderr, exit_code, elapsed_ms}. Non-zero exit_code is normal; only spawn / IO / timeout / bootstrap failures surface as tool errors.`),
		mcp.WithString("code", mcp.Required(), mcp.Description("Python source to execute. Multi-line allowed.")),
		mcp.WithNumber("timeout_ms", mcp.Description("Per-call deadline in ms. Default 300_000 (5 min); clamped to 7_200_000 (2 h) max.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return handleRun(ctx, req, deps, runRequest{kind: "run_code"})
	})
	srv.AddTool(mcp.NewTool("run_script",
		mcp.WithDescription(`Execute a Python script file in the per-session venv. The path is relative to the session workspace; absolute paths and ".." escapes are rejected. Result envelope is identical to run_code.`),
		mcp.WithString("path", mcp.Required(), mcp.Description("Script path relative to the session workspace.")),
		mcp.WithArray("args", mcp.Description("Optional argv passed to the script."),
			func(s map[string]any) { s["items"] = map[string]any{"type": "string"} }),
		mcp.WithNumber("timeout_ms", mcp.Description("Same semantics as run_code.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return handleRun(ctx, req, deps, runRequest{kind: "run_script"})
	})
}

// runRequest is the parsed-argument view of one call. Filled by
// handleRun's argument parser; passed to runPython.
type runRequest struct {
	kind      string // "run_code" or "run_script"
	code      string
	path      string
	scriptArg []string
	timeoutMs int64
}

func handleRun(ctx context.Context, req mcp.CallToolRequest, deps *execDeps, base runRequest) (*mcp.CallToolResult, error) {
	sid := sessionIDFromRequest(req)
	if sid == "" {
		return errResult(&toolError{Code: "arg_validation", Msg: "session_id missing in tool call metadata"}), nil
	}
	r := base
	if err := parseRunArgs(req, &r); err != nil {
		return errResult(err), nil
	}

	sessDir, sessVenv, err := ensureSessionVenv(deps, sid)
	if err != nil {
		return errResult(&toolError{Code: "venv_bootstrap_failed", Msg: err.Error()}), nil
	}

	url, token, err := deps.auth.currentToken(ctx)
	if err != nil {
		return errResult(&toolError{Code: "auth", Msg: err.Error()}), nil
	}

	out, terr := runPython(ctx, deps, r, sessDir, sessVenv, url, token)
	if terr != nil {
		return errResult(terr), nil
	}
	return okResult(out)
}

func parseRunArgs(req mcp.CallToolRequest, r *runRequest) error {
	args := req.GetArguments()
	if r.kind == "run_code" {
		c, ok := args["code"].(string)
		if !ok || c == "" {
			return &toolError{Code: "arg_validation", Msg: "code is required"}
		}
		r.code = c
	} else {
		p, ok := args["path"].(string)
		if !ok || p == "" {
			return &toolError{Code: "arg_validation", Msg: "path is required"}
		}
		r.path = p
		if rawArgs, ok := args["args"].([]any); ok {
			for _, v := range rawArgs {
				if s, ok := v.(string); ok {
					r.scriptArg = append(r.scriptArg, s)
				}
			}
		}
	}
	if t, ok := args["timeout_ms"].(float64); ok && t > 0 {
		r.timeoutMs = int64(t)
	}
	if r.timeoutMs <= 0 {
		r.timeoutMs = defaultTimeoutMs
	}
	if r.timeoutMs > maxTimeoutMs {
		r.timeoutMs = maxTimeoutMs
	}
	return nil
}

// ensureSessionVenv resolves <workspacesRoot>/<sid>/.venv and
// guarantees it is a complete copy of the template before
// returning. Fast path: a single os.Stat on the bootstrap stamp.
// Slow path: wipe partial dir, copyTree, write stamp.
func ensureSessionVenv(deps *execDeps, sid string) (sessDir, sessVenv string, err error) {
	sessDir = filepath.Join(deps.workspacesRoot, sid)
	sessVenv = filepath.Join(sessDir, ".venv")
	stamp := filepath.Join(sessVenv, stampName)

	if _, statErr := os.Stat(stamp); statErr == nil {
		return sessDir, sessVenv, nil
	}

	// Verify the template is itself usable. Without this every
	// session call returns the same generic copy error; pinpoint
	// the operator-side problem instead.
	if _, statErr := os.Stat(filepath.Join(deps.template, stampName)); statErr != nil {
		return "", "", fmt.Errorf("template missing or incomplete: %s", deps.template)
	}

	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		return "", "", fmt.Errorf("mkdir session dir: %w", err)
	}
	// Wipe any partial copy from a crashed previous attempt.
	if err := os.RemoveAll(sessVenv); err != nil {
		return "", "", fmt.Errorf("rm partial venv: %w", err)
	}
	if err := copyTree(deps.template, sessVenv); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(stamp, nil, 0o644); err != nil {
		return "", "", fmt.Errorf("write stamp: %w", err)
	}
	deps.log.Info("python-mcp: session venv ready",
		"session", sid, "venv", sessVenv)
	return sessDir, sessVenv, nil
}

// copyTree copies src into dst using the platform-appropriate CoW
// primitive. dst MUST not exist yet (ensureSessionVenv removes any
// partial copy first). Trailing /. on src copies *contents* of src,
// not the directory itself, so dst becomes a sibling of src.
func copyTree(src, dst string) error {
	var args []string
	switch runtime.GOOS {
	case "darwin":
		args = []string{"-cR", src + "/.", dst}
	case "linux":
		args = []string{"-R", "--reflink=auto", src + "/.", dst}
	default:
		args = []string{"-R", src + "/.", dst}
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("mkdir dst: %w", err)
	}
	cmd := exec.Command("cp", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("cp %v: %w (%s)", args, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// runPython spawns the per-call subprocess and captures bounded
// output. Returns a non-nil *toolError only for spawn / IO /
// timeout failures; non-zero exit codes go into the envelope.
func runPython(ctx context.Context, deps *execDeps, r runRequest, sessDir, sessVenv, hugrURL, hugrToken string) (runResult, *toolError) {
	pyBin := filepath.Join(sessVenv, "bin", "python")

	var cmd *exec.Cmd
	timeout := time.Duration(r.timeoutMs) * time.Millisecond
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	switch r.kind {
	case "run_code":
		cmd = exec.CommandContext(cctx, pyBin, "-c", r.code)
	case "run_script":
		scriptPath, err := resolveScriptPath(sessDir, r.path)
		if err != nil {
			return runResult{}, err
		}
		cmd = exec.CommandContext(cctx, pyBin, append([]string{scriptPath}, r.scriptArg...)...)
	default:
		return runResult{}, &toolError{Code: "io", Msg: "unknown tool kind: " + r.kind}
	}

	cmd.Dir = sessDir
	cmd.Env = composeChildEnv(hugrURL, hugrToken, sessDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var stdoutBuf, stderrBuf cappedBuffer
	stdoutBuf.cap = maxOutputBytes
	stderrBuf.cap = maxOutputBytes
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)

	res := runResult{
		Stdout:    stdoutBuf.String(),
		Stderr:    stderrBuf.String(),
		ElapsedMs: elapsed.Milliseconds(),
		Truncated: stdoutBuf.truncated || stderrBuf.truncated,
	}
	if err == nil {
		res.ExitCode = 0
		return res, nil
	}
	// Distinguish timeout from a normal non-zero exit. The context
	// deadline error trumps an exit-code result — the process was
	// killed because we ran out of time, not because the script
	// chose its own exit code.
	if cctx.Err() == context.DeadlineExceeded {
		killGroup(cmd)
		return runResult{}, &toolError{
			Code: "timeout",
			Msg:  fmt.Sprintf("exceeded %d ms", r.timeoutMs),
		}
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res.ExitCode = exitErr.ExitCode()
		return res, nil
	}
	return runResult{}, &toolError{Code: "io", Msg: err.Error()}
}

// resolveScriptPath enforces the session-workspace boundary
// documented in the contract: relative paths only, no `..` escape.
func resolveScriptPath(sessDir, requested string) (string, *toolError) {
	if filepath.IsAbs(requested) {
		return "", &toolError{Code: "arg_validation", Msg: "path must be relative to the session workspace"}
	}
	cleaned := filepath.Clean(requested)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", &toolError{Code: "arg_validation", Msg: "path escapes session workspace"}
	}
	abs := filepath.Join(sessDir, cleaned)
	if _, err := os.Stat(abs); err != nil {
		if os.IsNotExist(err) {
			return "", &toolError{Code: "not_found", Msg: fmt.Sprintf("script %s: no such file", requested)}
		}
		return "", &toolError{Code: "io", Msg: err.Error()}
	}
	return abs, nil
}

// composeChildEnv builds the env injected into every spawned
// Python subprocess. It deliberately drops HUGR_ACCESS_TOKEN /
// HUGR_TOKEN_URL (Go-side bootstrap secrets, not for user code)
// while forwarding everything else from the parent env.
func composeChildEnv(hugrURL, hugrToken, sessDir string) []string {
	parent := os.Environ()
	out := make([]string, 0, len(parent)+5)
	for _, kv := range parent {
		// strip secrets that should not leak into Python user code
		if strings.HasPrefix(kv, "HUGR_ACCESS_TOKEN=") || strings.HasPrefix(kv, "HUGR_TOKEN_URL=") {
			continue
		}
		// HUGR_URL gets re-set below from the auth source so a
		// drifted parent env doesn't override.
		if strings.HasPrefix(kv, "HUGR_URL=") {
			continue
		}
		// Same for HUGR_TOKEN — never inherit from parent.
		if strings.HasPrefix(kv, "HUGR_TOKEN=") {
			continue
		}
		out = append(out, kv)
	}
	if hugrURL != "" {
		out = append(out, "HUGR_URL="+hugrURL)
	}
	if hugrToken != "" {
		out = append(out, "HUGR_TOKEN="+hugrToken)
	}
	out = append(out,
		"PYTHONUNBUFFERED=1",
		"PYTHONDONTWRITEBYTECODE=1",
		"MPLBACKEND=Agg",
		"SESSION_DIR="+sessDir,
	)
	return out
}

// killGroup ensures the python child + any descendants started
// inside it die when we hit the deadline. Setpgid=true at spawn
// gives us a stable PG id to target.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// cappedBuffer captures up to cap bytes and silently drops the
// remainder, flagging Truncated. Used for stdout / stderr capture.
type cappedBuffer struct {
	buf       bytes.Buffer
	cap       int
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.buf.Len() >= c.cap {
		c.truncated = true
		return len(p), nil // pretend we accepted; child stays unblocked
	}
	room := c.cap - c.buf.Len()
	if len(p) <= room {
		return c.buf.Write(p)
	}
	c.buf.Write(p[:room])
	c.truncated = true
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	if !c.truncated {
		return c.buf.String()
	}
	return c.buf.String() + "\n[output truncated]"
}

// sessionIDFromRequest mirrors mcp/hugr-query's helper so test
// fixtures can stuff the id into AdditionalFields directly.
func sessionIDFromRequest(req mcp.CallToolRequest) string {
	if req.Params.Meta == nil {
		return ""
	}
	if v, ok := req.Params.Meta.AdditionalFields["session_id"]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func okResult(out runResult) (*mcp.CallToolResult, error) {
	body, err := json.Marshal(out)
	if err != nil {
		return errResult(&toolError{Code: "io", Msg: err.Error()}), nil
	}
	res := mcp.NewToolResultText(string(body))
	res.StructuredContent = out
	return res, nil
}

func errResult(err error) *mcp.CallToolResult {
	var te *toolError
	if !errors.As(err, &te) {
		te = &toolError{Code: "io", Msg: err.Error()}
	}
	body, _ := json.Marshal(te)
	res := mcp.NewToolResultErrorf("%s", string(body))
	res.StructuredContent = te
	return res
}

// Discard prevents a static-analysis warning for unused io import
// when we strip the non-buffer paths above. Kept tidy.
var _ = io.Discard
