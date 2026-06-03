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
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/mark3labs/mcp-go/mcp"
)

// Limits is the runtime knobs Tools needs.
type Limits struct {
	OutputMaxBytes int
	// ReadMaxBytes is the hard IO ceiling for a single read (default
	// 1 MB) — a memory bound, not a context bound.
	ReadMaxBytes int
	// ReadContextMaxBytes is the soft context cap: a read with no
	// explicit length, or one over this size, is truncated to it with
	// paging offsets so a large file never dumps whole into context.
	// Zero disables the soft cap (only the IO ceiling applies).
	ReadContextMaxBytes int
	DefaultTimeoutMS    int
	MemMB               int
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
		mcp.WithDescription("Execute a single binary by name with literal argv. NO shell — pipes (|), redirects (>, <), globs (*), variable expansion ($X), or chained commands (&&) DO NOT work here. Use bash.shell instead for any of those. cmd is the binary name, args is the argv slice."),
		mcp.WithString("cmd", mcp.Required(), mcp.Description("Binary name or absolute path. Must NOT contain shell syntax.")),
		mcp.WithArray("args", mcp.Description("Argv slice as individual strings — e.g. [\"-la\", \"src\"]. NOT a single command line."),
			mcp.WithStringItems()),
		mcp.WithString("cwd"),
		mcp.WithNumber("timeout_ms"),
		mcp.WithBoolean("shell"),
		mcp.WithObject("env"),
	), t.run)
	srv.AddTool(mcp.NewTool("bash.shell",
		mcp.WithDescription("Run a shell command line via sh -c. Supports pipes, redirects, globs, $VAR expansion, &&, etc. Use this for anything more than a plain binary call."),
		mcp.WithString("cmd", mcp.Required(), mcp.Description("Full shell command line — e.g. \"ls -R | grep foo\".")),
		mcp.WithString("cwd"),
		mcp.WithNumber("timeout_ms"),
		mcp.WithObject("env"),
	), t.shell)
	srv.AddTool(mcp.NewTool("bash.read_file",
		mcp.WithDescription("Read a file by path (resolved against the session workspace). A large file is truncated to a context-safe window (~16 KB) and the result carries truncated/next_start/bytes_total — pass start=next_start to page on, or use bash.grep to find just the part you need, or load it in python. Do NOT read a whole large file into context just to edit it."),
		mcp.WithString("path", mcp.Required()),
		mcp.WithNumber("start"),
		mcp.WithNumber("length"),
	), t.readFile)
	srv.AddTool(mcp.NewTool("bash.grep",
		mcp.WithDescription("Locate text in a file WITHOUT reading the whole file into context. Returns matching lines with line numbers and a few lines of surrounding context — enough to build the exact `old` string for bash.edit_file. Literal substring by default (no shell quoting); set regex=true for a Go regexp. Use this to find an anchor you already know (a label, a value, a heading) instead of read_file on a big file."),
		mcp.WithString("path", mcp.Required()),
		mcp.WithString("pattern", mcp.Required(), mcp.Description("Literal substring to find (or a Go regexp when regex=true).")),
		mcp.WithBoolean("regex", mcp.Description("Treat pattern as a Go regular expression. Default false (literal).")),
		mcp.WithNumber("context", mcp.Description("Lines of context to include around each match. Default 2.")),
		mcp.WithNumber("max_matches", mcp.Description("Cap on returned matches. Default 20.")),
	), t.grep)
	srv.AddTool(mcp.NewTool("bash.edit_file",
		mcp.WithDescription("Replace exact text in a file on disk WITHOUT pulling the file through context. Supply `old` (the minimal text being changed — e.g. just the value `$1,234,567.8`, not the whole line) and `new`. `old` must match exactly and must be unique unless replace_all=true or a `line` scope is given. The file content never enters context — you provide only the small strings you are changing. Errors: not_found (no match), ambiguous (>1 match without replace_all/line)."),
		mcp.WithString("path", mcp.Required()),
		mcp.WithString("old", mcp.Required(), mcp.Description("Exact text to replace — keep it minimal (just the changed token), not the whole line.")),
		mcp.WithString("new", mcp.Required(), mcp.Description("Replacement text.")),
		mcp.WithBoolean("replace_all", mcp.Description("Replace every occurrence. Default false (the match must be unique).")),
		mcp.WithNumber("line", mcp.Description("Optional 1-based line number (from bash.grep) to scope the match to a single line.")),
	), t.editFile)
	srv.AddTool(mcp.NewTool("bash.write_file",
		mcp.WithDescription("Write a file by path. Refuses /readonly/. MAX 10000 bytes of content per call — for more, write in chunks: one call, then more with mode=\"append\" (each ≤10000 bytes). Returns {bytes_written, size_total, path} — size_total is the file's full size after the write, the durable offset to resume an append sequence from."),
		mcp.WithString("path", mcp.Required()),
		mcp.WithString("content"),
		mcp.WithString("mode", mcp.Description(`"append" to add to the file (each call is a durable append — chunk a large document so a stall loses only the current chunk); anything else (default) truncates and overwrites.`)),
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
	cwd, err := os.Getwd()
	if err != nil {
		return errResult("io", err.Error()), nil
	}
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
		if isInternalEnvKey(envKey(kv)) {
			continue
		}
		out = append(out, kv)
	}
	// Apply the same filter to caller-supplied env: otherwise an
	// LLM could re-inject HUGR_ACCESS_TOKEN / HUGEN_AGENT_ID /
	// BASH_MCP_* via bash.run env override and reach back into
	// the agent's auth or workspace plumbing. The strip-on-inherit
	// pass above is only meaningful when symmetric here.
	for k, v := range extra {
		if isInternalEnvKey(k) {
			continue
		}
		out = append(out, k+"="+v)
	}
	return out
}

// envKey returns the leading "KEY" out of a "KEY=VALUE" pair.
// os.Environ entries always carry an `=` so the lookup is safe.
func envKey(kv string) string {
	if i := strings.IndexByte(kv, '='); i >= 0 {
		return kv[:i]
	}
	return kv
}

// isInternalEnvKey reports whether `name` is a runtime-private
// variable that must not cross the bash-mcp / child-process
// boundary in either direction.
func isInternalEnvKey(name string) bool {
	return strings.HasPrefix(name, "HUGR_") ||
		strings.HasPrefix(name, "HUGEN_") ||
		strings.HasPrefix(name, "BASH_MCP_")
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
	var fileSize int64 = -1
	if fi, statErr := f.Stat(); statErr == nil {
		fileSize = fi.Size()
	}
	if a.Start > 0 {
		if _, err := f.Seek(a.Start, io.SeekStart); err != nil {
			return errResult("io", err.Error()), nil
		}
	}
	// Two distinct caps. ReadMaxBytes is the hard IO ceiling (memory
	// bound). ReadContextMaxBytes is the soft context cap: a read with
	// no explicit length, or an explicit length above the cap, is
	// truncated to it so a large file never dumps whole into context.
	// An explicit length at or under the cap is honoured as-is
	// (deliberate windowing).
	readCap := a.Length
	if readCap <= 0 {
		readCap = int64(t.Limits.ReadMaxBytes)
	}
	cappedByCtx := false
	if ctxCap := int64(t.Limits.ReadContextMaxBytes); ctxCap > 0 && readCap > ctxCap {
		readCap = ctxCap
		cappedByCtx = true
	}
	if rm := int64(t.Limits.ReadMaxBytes); rm > 0 && readCap > rm {
		readCap = rm
	}
	buf := make([]byte, readCap)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return errResult("io", err.Error()), nil
	}
	buf = buf[:n]
	atEOF := errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || int64(n) < readCap
	body := map[string]any{
		"bytes_read": n,
		"eof":        atEOF,
	}
	if utf8.Valid(buf) {
		body["content"] = string(buf)
	} else {
		body["content_b64"] = encodeB64(buf)
	}
	// Soft-truncation marker: only when the context cap clipped the
	// read AND there is more file past what we returned.
	hasMore := !atEOF
	if fileSize >= 0 {
		hasMore = a.Start+int64(n) < fileSize
	}
	if cappedByCtx && hasMore {
		nextStart := a.Start + int64(n)
		body["truncated"] = true
		body["next_start"] = nextStart
		if fileSize >= 0 {
			body["bytes_total"] = fileSize - a.Start
		}
		note := fmt.Sprintf("showing %d bytes from offset %d", n, a.Start)
		if fileSize >= 0 {
			note = fmt.Sprintf("showing %d of %d bytes from offset %d", n, fileSize-a.Start, a.Start)
		}
		note += fmt.Sprintf("; pass start=%d to continue, or bash.grep to find the part you need, or load it in python — do not read the whole file into context.", nextStart)
		body["note"] = note
		return jsonResultNote(body, note), nil
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

// maxWriteChunkBytes caps how much a single write_file call may emit.
// The model generates the `content` token-by-token, so a multi-KB
// write is a long, wedge-prone stream — capping it forces large output
// into several short, resumable append calls (a stall loses only the
// current chunk; size_total tracks the durable offset).
const maxWriteChunkBytes = 10000

func (t *Tools) writeFile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a writeArgs
	if err := req.BindArguments(&a); err != nil {
		return errResult("arg_validation", err.Error()), nil
	}
	if a.Path == "" {
		return errResult("arg_validation", "path required"), nil
	}
	if len(a.Content) > maxWriteChunkBytes {
		return errResult("arg_validation", fmt.Sprintf(
			"content is %d bytes; the per-call limit is %d. Write large output in chunks: one write_file, then more with mode=\"append\" (each ≤ %d bytes). For a big generated document or script, prefer python (write the file from inside run_script) so the data never streams through the model.",
			len(a.Content), maxWriteChunkBytes, maxWriteChunkBytes)), nil
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
	// size_total is the file's full size AFTER this write — for a
	// chunked `mode:"append"` sequence it is the durable offset the
	// model resumes from, so a stall mid-document doesn't force a
	// re-read to learn how much already landed. Best-effort: omit it
	// if the stat fails rather than report a misleading zero.
	if fi, statErr := f.Stat(); statErr == nil {
		body["size_total"] = fi.Size()
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
	walk := func(_ string, d fs.DirEntry) error {
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

// ----- bash.grep -----

type grepArgs struct {
	Path       string `json:"path"`
	Pattern    string `json:"pattern"`
	Regex      bool   `json:"regex,omitempty"`
	Context    *int   `json:"context,omitempty"`
	MaxMatches int    `json:"max_matches,omitempty"`
}

// maxGrepLineBytes bounds how much of any single emitted line grep
// returns. A minified file can hold the whole document on one line; the
// matched line is clipped to a window centred on the match so grep stays
// useful (and context-cheap) even then.
const maxGrepLineBytes = 400

func (t *Tools) grep(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a grepArgs
	if err := req.BindArguments(&a); err != nil {
		return errResult("arg_validation", err.Error()), nil
	}
	if a.Path == "" || a.Pattern == "" {
		return errResult("arg_validation", "path and pattern required"), nil
	}
	res, err := t.WS.Resolve(a.Path, false)
	if err != nil {
		return errResult(errCode(err), err.Error()), nil
	}
	data, err := os.ReadFile(res.Canonical)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return errResult("not_found", res.Logical), nil
		}
		return errResult("io", err.Error()), nil
	}
	cw := 2
	if a.Context != nil {
		cw = *a.Context
		if cw < 0 {
			cw = 0
		}
	}
	maxM := a.MaxMatches
	if maxM <= 0 {
		maxM = 20
	}
	var re *regexp.Regexp
	if a.Regex {
		re, err = regexp.Compile(a.Pattern)
		if err != nil {
			return errResult("arg_validation", "invalid regex: "+err.Error()), nil
		}
	}
	lines := strings.Split(string(data), "\n")
	type grepMatch struct {
		LineNo int    `json:"line_no"`
		Text   string `json:"text"`
	}
	var matches []grepMatch
	truncated := false
	outBytes := 0
	for i, line := range lines {
		off := -1
		if a.Regex {
			if loc := re.FindStringIndex(line); loc != nil {
				off = loc[0]
			}
		} else {
			off = strings.Index(line, a.Pattern)
		}
		if off < 0 {
			continue
		}
		if len(matches) >= maxM {
			truncated = true
			break
		}
		lo := i - cw
		if lo < 0 {
			lo = 0
		}
		hi := i + cw
		if hi >= len(lines) {
			hi = len(lines) - 1
		}
		// Clip each line in the window; centre the matched line on the
		// hit so the anchor is always visible.
		parts := make([]string, 0, hi-lo+1)
		for j := lo; j <= hi; j++ {
			if j == i {
				parts = append(parts, clipLine(lines[j], off, maxGrepLineBytes))
			} else {
				parts = append(parts, clipLine(lines[j], -1, maxGrepLineBytes))
			}
		}
		text := strings.Join(parts, "\n")
		if t.Limits.OutputMaxBytes > 0 && outBytes+len(text) > t.Limits.OutputMaxBytes {
			truncated = true
			break
		}
		outBytes += len(text)
		matches = append(matches, grepMatch{LineNo: i + 1, Text: text})
	}
	body := map[string]any{
		"matches":     matches,
		"match_count": len(matches),
		"truncated":   truncated,
	}
	return jsonResult(body), nil
}

// clipLine bounds a line to cap bytes. When off >= 0 (the matched line)
// the window is centred on the match so the anchor stays visible;
// otherwise the head is kept. Ellipses mark where content was dropped.
func clipLine(line string, off, limit int) string {
	if len(line) <= limit {
		return line
	}
	if off < 0 {
		return line[:limit] + "…"
	}
	start := off - limit/2
	if start < 0 {
		start = 0
	}
	end := start + limit
	if end > len(line) {
		end = len(line)
		start = end - limit
		if start < 0 {
			start = 0
		}
	}
	s := line[start:end]
	if start > 0 {
		s = "…" + s
	}
	if end < len(line) {
		s += "…"
	}
	return s
}

// ----- bash.edit_file -----

type editArgs struct {
	Path       string `json:"path"`
	Old        string `json:"old"`
	New        string `json:"new"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
	Line       *int   `json:"line,omitempty"`
}

func (t *Tools) editFile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a editArgs
	if err := req.BindArguments(&a); err != nil {
		return errResult("arg_validation", err.Error()), nil
	}
	if a.Path == "" {
		return errResult("arg_validation", "path required"), nil
	}
	if a.Old == "" {
		return errResult("arg_validation", "old required (the exact text to replace)"), nil
	}
	if a.Old == a.New {
		return errResult("arg_validation", "old and new are identical — nothing to change"), nil
	}
	res, err := t.WS.Resolve(a.Path, true)
	if err != nil {
		return errResult(errCode(err), err.Error()), nil
	}
	data, err := os.ReadFile(res.Canonical)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return errResult("not_found", res.Logical), nil
		}
		return errResult("io", err.Error()), nil
	}
	content := string(data)

	var newContent string
	var replacements int
	if a.Line != nil {
		lines := strings.Split(content, "\n")
		idx := *a.Line - 1
		if idx < 0 || idx >= len(lines) {
			return errResult("arg_validation", fmt.Sprintf("line %d out of range (file has %d lines)", *a.Line, len(lines))), nil
		}
		count := strings.Count(lines[idx], a.Old)
		if count == 0 {
			return errResult("not_found", fmt.Sprintf("old not found on line %d", *a.Line)), nil
		}
		if count > 1 && !a.ReplaceAll {
			return errResult("ambiguous", fmt.Sprintf("old matches %d times on line %d; pass replace_all=true", count, *a.Line)), nil
		}
		if a.ReplaceAll {
			lines[idx] = strings.ReplaceAll(lines[idx], a.Old, a.New)
			replacements = count
		} else {
			lines[idx] = strings.Replace(lines[idx], a.Old, a.New, 1)
			replacements = 1
		}
		newContent = strings.Join(lines, "\n")
	} else {
		count := strings.Count(content, a.Old)
		if count == 0 {
			return errResult("not_found", "old not found"), nil
		}
		if count > 1 && !a.ReplaceAll {
			return errResult("ambiguous", fmt.Sprintf("old matches %d times; pass replace_all=true, a more specific old, or a line scope", count)), nil
		}
		if a.ReplaceAll {
			newContent = strings.ReplaceAll(content, a.Old, a.New)
			replacements = count
		} else {
			newContent = strings.Replace(content, a.Old, a.New, 1)
			replacements = 1
		}
	}
	if err := os.WriteFile(res.Canonical, []byte(newContent), 0o644); err != nil {
		return errResult("io", err.Error()), nil
	}
	body := map[string]any{
		"replacements": replacements,
		"path":         res.Logical,
	}
	return jsonResult(body), nil
}

// ----- helpers -----

func errCode(err error) string {
	switch {
	case errors.Is(err, ErrCrossSessionPath):
		return "cross_session"
	case errors.Is(err, ErrPathEscape):
		return "path_escape"
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

// jsonResultNote is jsonResult with a human-readable note led in front
// of the JSON text, so a model reading the text content sees the
// guidance first. StructuredContent stays the raw body.
func jsonResultNote(body any, note string) *mcp.CallToolResult {
	enc, _ := json.Marshal(body)
	res := mcp.NewToolResultText(note + "\n" + string(enc))
	res.StructuredContent = body
	return res
}

func encodeB64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
