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
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// execDeps groups the per-server immutable dependencies the tool
// handlers consult on every call. Constructed once in main, passed
// to registerTools. The session_dir comes per call via MCP
// `_meta.session_dir` (resolved by the runtime's workspace
// extension) — there is no agent-wide workspaces root cached here.
type execDeps struct {
	template string // absolute path to the relocatable template venv
	auth     *authSource
	log      *slog.Logger

	// bootstrapMu serialises venv bootstrap per session-dir within
	// this single python-mcp process. Two workers in the same
	// mission share their session_dir under 5.4 — without this map
	// they could both pass the stamp check, both wipe + recopy, and
	// race on the partial .venv state. We're per_agent (one
	// process), so an in-memory mutex suffices.
	bootstrapMu   sync.Mutex
	bootstrapLock map[string]*sync.Mutex
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
		mcp.WithDescription(`Execute Python source in the per-session venv. Returns {stdout, stderr, exit_code, elapsed_ms}. Non-zero exit_code is normal; only spawn / IO / timeout / bootstrap failures surface as tool errors. MAX 10000 bytes of code — for a bigger script write it to a .py file (bash.write_file, ≤10000-byte append chunks) and use run_script.`),
		mcp.WithString("code", mcp.Required(), mcp.Description("Python source to execute. Multi-line allowed. Max 10000 bytes; larger → write a file + run_script.")),
		mcp.WithNumber("timeout_ms", mcp.Description("Per-call deadline in ms. Default 300_000 (5 min); clamped to 7_200_000 (2 h) max.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return handleRun(ctx, req, deps, runRequest{kind: "run_code"})
	})
	srv.AddTool(mcp.NewTool("run_script",
		mcp.WithDescription(`Execute a Python script file in the per-session venv. The path is relative to the session workspace; absolute paths and ".." escapes are rejected. Positional argv comes from "args"; "kwargs" expands sorted by key as --key value pairs after the positional argv. Result envelope is identical to run_code.`),
		mcp.WithString("path", mcp.Required(), mcp.Description("Script path relative to the session workspace.")),
		mcp.WithArray("args", mcp.Description("Optional positional argv passed to the script."),
			func(s map[string]any) { s["items"] = map[string]any{"type": "string"} }),
		mcp.WithObject("kwargs", mcp.Description("Optional keyword args expanded as --key value pairs, sorted by key, appended after positional argv. Values must be strings; the script does its own parsing."),
			func(s map[string]any) { s["additionalProperties"] = map[string]any{"type": "string"} }),
		mcp.WithNumber("timeout_ms", mcp.Description("Same semantics as run_code.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return handleRun(ctx, req, deps, runRequest{kind: "run_script"})
	})
}

// runRequest is the parsed-argument view of one call. Filled by
// handleRun's argument parser; passed to runPython.
type runRequest struct {
	kind        string // "run_code" or "run_script"
	code        string
	path        string
	scriptArg   []string
	scriptKwarg map[string]string
	timeoutMs   int64
}

func handleRun(ctx context.Context, req mcp.CallToolRequest, deps *execDeps, base runRequest) (*mcp.CallToolResult, error) {
	sessDir := sessionDirFromRequest(req)
	if sessDir == "" {
		return errResult(&toolError{Code: "arg_validation", Msg: "session_dir missing in tool call metadata"}), nil
	}
	r := base
	if err := parseRunArgs(req, &r); err != nil {
		return errResult(err), nil
	}

	sessVenv, err := ensureSessionVenv(deps, sessDir)
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

// maxRunCodeBytes caps inline run_code source. The model generates
// `code` token-by-token, so a multi-KB inline script is a long,
// wedge-prone stream. Past this, write the script to a .py file
// (bash.write_file in ≤10000-byte append chunks) and run_script it —
// the execution itself streams nothing.
const maxRunCodeBytes = 10000

func parseRunArgs(req mcp.CallToolRequest, r *runRequest) error {
	args := req.GetArguments()
	if r.kind == "run_code" {
		c, ok := args["code"].(string)
		if !ok || c == "" {
			return &toolError{Code: "arg_validation", Msg: "code is required"}
		}
		if len(c) > maxRunCodeBytes {
			return &toolError{Code: "arg_validation", Msg: fmt.Sprintf(
				"code is %d bytes; the run_code per-call limit is %d. For a larger script, write it to a .py file (bash.write_file in ≤%d-byte chunks with mode=\"append\") and execute it with run_script — a long inline generation is wedge-prone.",
				len(c), maxRunCodeBytes, maxRunCodeBytes)}
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
		if rawKwargs, ok := args["kwargs"].(map[string]any); ok {
			r.scriptKwarg = make(map[string]string, len(rawKwargs))
			for k, v := range rawKwargs {
				if !kwargKeyRe.MatchString(k) {
					return &toolError{Code: "arg_validation", Msg: fmt.Sprintf("kwargs key %q must match [A-Za-z_][A-Za-z0-9_-]*", k)}
				}
				s, ok := v.(string)
				if !ok {
					return &toolError{Code: "arg_validation", Msg: fmt.Sprintf("kwargs.%s must be a string", k)}
				}
				r.scriptKwarg[k] = s
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

// ensureSessionVenv resolves <sessDir>/.venv and guarantees it is
// a complete copy of the template before returning. Fast path: a
// single os.Stat on the bootstrap stamp. Slow path: take a per-dir
// mutex (so siblings sharing the same mission folder under 5.4
// don't race on wipe+recopy), wipe partial dir, copyTree, write
// stamp.
func ensureSessionVenv(deps *execDeps, sessDir string) (sessVenv string, err error) {
	sessVenv = filepath.Join(sessDir, ".venv")
	stamp := filepath.Join(sessVenv, stampName)

	// Fast path — stamp present, no lock needed.
	if _, statErr := os.Stat(stamp); statErr == nil {
		return sessVenv, nil
	}

	// Acquire a per-session-dir bootstrap mutex. Held only across
	// the (potentially slow) copyTree path; a sibling worker that
	// arrived 100 ms later will re-stat the stamp on the way in and
	// return immediately.
	lk := deps.acquireBootstrapLock(sessDir)
	lk.Lock()
	defer lk.Unlock()

	// Re-check under lock — earlier holder may have finished
	// bootstrap while we were queued.
	if _, statErr := os.Stat(stamp); statErr == nil {
		return sessVenv, nil
	}

	// Verify the template is itself usable. Without this every
	// session call returns the same generic copy error; pinpoint
	// the operator-side problem instead.
	if _, statErr := os.Stat(filepath.Join(deps.template, stampName)); statErr != nil {
		return "", fmt.Errorf("template missing or incomplete: %s", deps.template)
	}

	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir session dir: %w", err)
	}
	// Wipe any partial copy from a crashed previous attempt.
	if err := os.RemoveAll(sessVenv); err != nil {
		return "", fmt.Errorf("rm partial venv: %w", err)
	}
	if err := copyTree(deps.template, sessVenv); err != nil {
		return "", err
	}
	if err := os.WriteFile(stamp, nil, 0o644); err != nil {
		return "", fmt.Errorf("write stamp: %w", err)
	}
	deps.log.Info("python-mcp: session venv ready",
		"session_dir", sessDir, "venv", sessVenv)
	return sessVenv, nil
}

// acquireBootstrapLock returns the per-session-dir mutex, creating
// it on first call. Lazy init under bootstrapMu keeps the map
// concurrency-safe without a separate constructor.
func (deps *execDeps) acquireBootstrapLock(sessDir string) *sync.Mutex {
	deps.bootstrapMu.Lock()
	defer deps.bootstrapMu.Unlock()
	if deps.bootstrapLock == nil {
		deps.bootstrapLock = make(map[string]*sync.Mutex)
	}
	if mu, ok := deps.bootstrapLock[sessDir]; ok {
		return mu
	}
	mu := &sync.Mutex{}
	deps.bootstrapLock[sessDir] = mu
	return mu
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
		argv := append([]string{scriptPath}, r.scriptArg...)
		argv = append(argv, flattenKwargs(r.scriptKwarg)...)
		cmd = exec.CommandContext(cctx, pyBin, argv...)
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

// kwargKeyRe rejects keys that would mangle argv when expanded into
// `--<key>`: empty (`--` is POSIX end-of-options), leading `-`
// (`---x`), `=` (collapses key+value into one token), or anything
// outside identifier-ish chars. Mirrors argparse's allowed `dest`
// shape with a `-` concession for `--my-flag` style.
var kwargKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

// flattenKwargs emits a sorted `--key value` argv tail. Sorting is
// load-bearing: the cron-task checker compares command shapes across
// fires, so map iteration order would create spurious diffs.
func flattenKwargs(kw map[string]string) []string {
	if len(kw) == 0 {
		return nil
	}
	keys := make([]string, 0, len(kw))
	for k := range kw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(kw)*2)
	for _, k := range keys {
		out = append(out, "--"+k, kw[k])
	}
	return out
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

// sessionDirFromRequest reads the resolved workspace directory the
// runtime injects via MCP `_meta.session_dir`. Tests can put the
// path directly into AdditionalFields.
func sessionDirFromRequest(req mcp.CallToolRequest) string {
	if req.Params.Meta == nil {
		return ""
	}
	if v, ok := req.Params.Meta.AdditionalFields["session_dir"]; ok {
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
