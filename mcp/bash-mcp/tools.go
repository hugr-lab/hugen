package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/mark3labs/mcp-go/mcp"
)

// Limits is the runtime knobs Tools needs.
type Limits struct {
	OutputMaxBytes   int
	ReadMaxBytes     int
	DefaultTimeoutMS int
	MemMB            int
}

// Tools wires the bash.* tool set to a Workspace + Limits. All
// handlers are stateless; concurrency is the MCP server's
// responsibility.
type Tools struct {
	WS     *Workspace
	Limits Limits
}

// Register attaches every bash.* tool to srv.
func (t *Tools) Register(srv mcpToolRegistrar) {
	srv.AddTool(mcp.NewTool("bash.run",
		mcp.WithDescription("Execute a non-interactive command in the session workspace."),
		mcp.WithString("cmd", mcp.Required()),
		mcp.WithArray("args"),
		mcp.WithString("cwd"),
		mcp.WithNumber("timeout_ms"),
		mcp.WithBoolean("shell"),
		mcp.WithObject("env"),
	), t.run)
	srv.AddTool(mcp.NewTool("bash.shell",
		mcp.WithDescription("Run a shell command (sh -c) in the session workspace."),
		mcp.WithString("cmd", mcp.Required()),
		mcp.WithString("cwd"),
		mcp.WithNumber("timeout_ms"),
		mcp.WithObject("env"),
	), t.shell)
	srv.AddTool(mcp.NewTool("bash.read_file",
		mcp.WithDescription("Read a file by path (resolved against the session workspace)."),
		mcp.WithString("path", mcp.Required()),
		mcp.WithNumber("start"),
		mcp.WithNumber("length"),
	), t.readFile)
	srv.AddTool(mcp.NewTool("bash.write_file",
		mcp.WithDescription("Write a file by path. Refuses /readonly/."),
		mcp.WithString("path", mcp.Required()),
		mcp.WithString("content"),
		mcp.WithString("mode"),
		mcp.WithBoolean("mkdir_parents"),
	), t.writeFile)
	srv.AddTool(mcp.NewTool("bash.list_dir",
		mcp.WithDescription("List directory entries."),
		mcp.WithString("path", mcp.Required()),
		mcp.WithString("glob"),
		mcp.WithBoolean("recursive"),
		mcp.WithNumber("max_entries"),
	), t.listDir)
	srv.AddTool(mcp.NewTool("bash.sed",
		mcp.WithDescription("Apply an in-place sed script to a file under the session workspace."),
		mcp.WithString("path", mcp.Required()),
		mcp.WithString("script", mcp.Required()),
		mcp.WithBoolean("in_place"),
	), t.sed)
}

// mcpToolRegistrar is the subset of *server.MCPServer the Tools
// helper depends on. Defined here so we can stub it in tests
// without dragging the server package in.
type mcpToolRegistrar interface {
	AddTool(tool mcp.Tool, handler func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error))
}

// ----- bash.run / bash.shell -----

type runArgs struct {
	Cmd       string            `json:"cmd"`
	Args      []string          `json:"args,omitempty"`
	Cwd       string            `json:"cwd,omitempty"`
	TimeoutMS int               `json:"timeout_ms,omitempty"`
	Shell     bool              `json:"shell,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
}

func (t *Tools) run(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a runArgs
	if err := req.BindArguments(&a); err != nil {
		return errResult("arg_validation", err.Error()), nil
	}
	return t.runCore(ctx, a)
}

func (t *Tools) shell(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a runArgs
	if err := req.BindArguments(&a); err != nil {
		return errResult("arg_validation", err.Error()), nil
	}
	a.Shell = true
	return t.runCore(ctx, a)
}

func (t *Tools) runCore(ctx context.Context, a runArgs) (*mcp.CallToolResult, error) {
	if a.Cmd == "" {
		return errResult("arg_validation", "cmd required"), nil
	}
	cwd := t.WS.WorkspaceRoot + "/" + t.WS.SessionID
	if a.Cwd != "" {
		res, err := t.WS.Resolve(a.Cwd, false)
		if err != nil {
			return errResult(errCode(err), err.Error()), nil
		}
		cwd = res.Canonical
	}
	timeout := a.TimeoutMS
	if timeout <= 0 {
		timeout = t.Limits.DefaultTimeoutMS
	}
	command := a.Cmd
	args := a.Args
	if a.Shell {
		args = []string{"-c", a.Cmd}
		command = "sh"
	}
	envList := buildEnv(a.Env)
	out, err := runProcess(ctx, RunOptions{
		Command:   command,
		Args:      args,
		Cwd:       cwd,
		Env:       envList,
		TimeoutMS: timeout,
		MemMB:     t.Limits.MemMB,
		OutputCap: t.Limits.OutputMaxBytes,
	})
	if err != nil {
		return errResult("io", err.Error()), nil
	}
	if out.TimedOut {
		return errResult("timeout", fmt.Sprintf("elapsed=%dms", out.ElapsedMS)), nil
	}
	body := map[string]any{
		"exit_code":  out.ExitCode,
		"stdout":     out.Stdout,
		"stderr":     out.Stderr,
		"elapsed_ms": out.ElapsedMS,
		"truncated":  out.Truncated,
	}
	return jsonResult(body), nil
}

func buildEnv(extra map[string]string) []string {
	out := make([]string, 0, len(extra)+8)
	for _, kv := range os.Environ() {
		// Strip our own internal vars before forwarding to the child.
		if strings.HasPrefix(kv, "HUGR_") || strings.HasPrefix(kv, "HUGEN_") || strings.HasPrefix(kv, "BASH_MCP_") {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range extra {
		out = append(out, k+"="+v)
	}
	return out
}

// ----- bash.read_file -----

type readArgs struct {
	Path   string `json:"path"`
	Start  int64  `json:"start,omitempty"`
	Length int64  `json:"length,omitempty"`
}

func (t *Tools) readFile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a readArgs
	if err := req.BindArguments(&a); err != nil {
		return errResult("arg_validation", err.Error()), nil
	}
	if a.Path == "" {
		return errResult("arg_validation", "path required"), nil
	}
	res, err := t.WS.Resolve(a.Path, false)
	if err != nil {
		return errResult(errCode(err), err.Error()), nil
	}
	f, err := os.Open(res.Canonical)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return errResult("not_found", res.Logical), nil
		}
		return errResult("io", err.Error()), nil
	}
	defer f.Close()
	if a.Start > 0 {
		if _, err := f.Seek(a.Start, io.SeekStart); err != nil {
			return errResult("io", err.Error()), nil
		}
	}
	max := a.Length
	if max <= 0 {
		max = int64(t.Limits.ReadMaxBytes)
	}
	buf := make([]byte, max)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return errResult("io", err.Error()), nil
	}
	buf = buf[:n]
	body := map[string]any{
		"bytes_read": n,
		"eof":        errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || int64(n) < max,
	}
	if utf8.Valid(buf) {
		body["content"] = string(buf)
	} else {
		body["content_b64"] = encodeB64(buf)
	}
	return jsonResult(body), nil
}

// ----- bash.write_file -----

type writeArgs struct {
	Path         string `json:"path"`
	Content      string `json:"content"`
	Mode         string `json:"mode,omitempty"`
	MkdirParents bool   `json:"mkdir_parents,omitempty"`
}

func (t *Tools) writeFile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a writeArgs
	if err := req.BindArguments(&a); err != nil {
		return errResult("arg_validation", err.Error()), nil
	}
	if a.Path == "" {
		return errResult("arg_validation", "path required"), nil
	}
	res, err := t.WS.Resolve(a.Path, true)
	if err != nil {
		return errResult(errCode(err), err.Error()), nil
	}
	parent := filepath.Dir(res.Canonical)
	if a.MkdirParents {
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return errResult("io", err.Error()), nil
		}
	}
	flag := os.O_CREATE | os.O_WRONLY
	if a.Mode == "append" {
		flag |= os.O_APPEND
	} else {
		flag |= os.O_TRUNC
	}
	f, err := os.OpenFile(res.Canonical, flag, 0o644)
	if err != nil {
		return errResult("io", err.Error()), nil
	}
	defer f.Close()
	n, err := f.WriteString(a.Content)
	if err != nil {
		return errResult("io", err.Error()), nil
	}
	body := map[string]any{
		"bytes_written": n,
		"path":          res.Logical,
	}
	return jsonResult(body), nil
}

// ----- bash.list_dir -----

type listArgs struct {
	Path       string `json:"path"`
	Glob       string `json:"glob,omitempty"`
	Recursive  bool   `json:"recursive,omitempty"`
	MaxEntries int    `json:"max_entries,omitempty"`
}

func (t *Tools) listDir(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a listArgs
	if err := req.BindArguments(&a); err != nil {
		return errResult("arg_validation", err.Error()), nil
	}
	if a.Path == "" {
		a.Path = "."
	}
	res, err := t.WS.Resolve(a.Path, false)
	if err != nil {
		return errResult(errCode(err), err.Error()), nil
	}
	if a.MaxEntries <= 0 {
		a.MaxEntries = 1000
	}
	type entry struct {
		Name       string `json:"name"`
		Size       int64  `json:"size"`
		IsDir      bool   `json:"is_dir"`
		ModifiedAt string `json:"modified_at"`
	}
	var entries []entry
	truncated := false
	walk := func(path string, d fs.DirEntry) error {
		if len(entries) >= a.MaxEntries {
			truncated = true
			return io.EOF
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		if a.Glob != "" {
			match, _ := filepath.Match(a.Glob, fi.Name())
			if !match {
				return nil
			}
		}
		entries = append(entries, entry{
			Name:       fi.Name(),
			Size:       fi.Size(),
			IsDir:      fi.IsDir(),
			ModifiedAt: fi.ModTime().UTC().Format("2006-01-02T15:04:05Z"),
		})
		return nil
	}
	if a.Recursive {
		_ = filepath.WalkDir(res.Canonical, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if path == res.Canonical {
				return nil
			}
			return walk(path, d)
		})
	} else {
		des, err := os.ReadDir(res.Canonical)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return errResult("not_found", res.Logical), nil
			}
			return errResult("io", err.Error()), nil
		}
		for _, d := range des {
			if err := walk(filepath.Join(res.Canonical, d.Name()), d); err != nil {
				break
			}
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	body := map[string]any{"entries": entries, "truncated": truncated}
	return jsonResult(body), nil
}

// ----- bash.sed -----

type sedArgs struct {
	Path    string `json:"path"`
	Script  string `json:"script"`
	InPlace *bool  `json:"in_place,omitempty"`
}

func (t *Tools) sed(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a sedArgs
	if err := req.BindArguments(&a); err != nil {
		return errResult("arg_validation", err.Error()), nil
	}
	if a.Path == "" || a.Script == "" {
		return errResult("arg_validation", "path and script required"), nil
	}
	res, err := t.WS.Resolve(a.Path, true)
	if err != nil {
		return errResult(errCode(err), err.Error()), nil
	}
	original, err := os.ReadFile(res.Canonical)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return errResult("not_found", res.Logical), nil
		}
		return errResult("io", err.Error()), nil
	}
	out, err := runProcess(ctx, RunOptions{
		Command:   "sed",
		Args:      []string{a.Script},
		TimeoutMS: t.Limits.DefaultTimeoutMS,
		OutputCap: t.Limits.OutputMaxBytes,
		Stdin:     string(original),
	})
	if err != nil {
		return errResult("io", err.Error()), nil
	}
	if out.ExitCode != 0 {
		return errResult("io", strings.TrimSpace(out.Stderr)), nil
	}
	inPlace := true
	if a.InPlace != nil {
		inPlace = *a.InPlace
	}
	if inPlace {
		if err := os.WriteFile(res.Canonical, []byte(out.Stdout), 0o644); err != nil {
			return errResult("io", err.Error()), nil
		}
	}
	body := map[string]any{
		"bytes_changed": len(out.Stdout) - len(original),
		"path":          res.Logical,
	}
	return jsonResult(body), nil
}

// ----- helpers -----

func errCode(err error) string {
	switch {
	case errors.Is(err, ErrPathEscape):
		return "path_escape"
	case errors.Is(err, ErrReadOnly):
		return "readonly"
	case errors.Is(err, fs.ErrNotExist):
		return "not_found"
	default:
		return "io"
	}
}

func errResult(code, msg string) *mcp.CallToolResult {
	body, _ := json.Marshal(map[string]any{"code": code, "message": msg})
	res := mcp.NewToolResultText(string(body))
	res.IsError = true
	return res
}

func jsonResult(body any) *mcp.CallToolResult {
	enc, _ := json.Marshal(body)
	res := mcp.NewToolResultText(string(enc))
	res.StructuredContent = body
	return res
}

func encodeB64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
