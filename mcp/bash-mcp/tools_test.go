package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

func newTools(t *testing.T) (*Tools, *Workspace) {
	t.Helper()
	chdirTo(t, t.TempDir())
	w := &Workspace{}
	return &Tools{
		WS: w,
		Limits: Limits{
			OutputMaxBytes:   32 * 1024,
			ReadMaxBytes:     1024 * 1024,
			DefaultTimeoutMS: 5000,
		},
	}, w
}

func callJSON(t *testing.T, fn func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error), args map[string]any) (map[string]any, *mcp.CallToolResult) {
	t.Helper()
	raw, _ := json.Marshal(args)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = json.RawMessage(raw)
	res, err := fn(context.Background(), req)
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if res.StructuredContent == nil {
		// Error path: parse the text content.
		if len(res.Content) == 0 {
			t.Fatalf("no content")
		}
		tc := res.Content[0].(mcp.TextContent)
		var out map[string]any
		if err := json.Unmarshal([]byte(tc.Text), &out); err != nil {
			t.Fatalf("decode: %v\n%s", err, tc.Text)
		}
		return out, res
	}
	body, _ := json.Marshal(res.StructuredContent)
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out, res
}

func TestTools_WriteThenRead(t *testing.T) {
	tools, _ := newTools(t)
	out, res := callJSON(t, tools.writeFile, map[string]any{
		"path":    "out.txt",
		"content": "hello world",
	})
	if res.IsError {
		t.Fatalf("write IsError: %v", out)
	}
	if int(out["bytes_written"].(float64)) != 11 {
		t.Errorf("bytes_written = %v", out["bytes_written"])
	}
	out2, res2 := callJSON(t, tools.readFile, map[string]any{"path": "out.txt"})
	if res2.IsError {
		t.Fatalf("read IsError: %v", out2)
	}
	if out2["content"] != "hello world" {
		t.Errorf("content = %v", out2["content"])
	}
}

// TestTools_WriteAppend_SizeTotal verifies that mode:"append" adds to
// the file rather than truncating, and that size_total reports the
// file's full size after each write — the durable offset a chunked
// report-builder fallback resumes from after a stall (B20).
func TestTools_WriteAppend_SizeTotal(t *testing.T) {
	tools, _ := newTools(t)

	// First chunk: a fresh write reports bytes_written == size_total.
	out, res := callJSON(t, tools.writeFile, map[string]any{
		"path":    "doc.html",
		"content": "<h1>part1</h1>",
	})
	if res.IsError {
		t.Fatalf("write chunk 1 IsError: %v", out)
	}
	if int(out["bytes_written"].(float64)) != 14 {
		t.Errorf("chunk 1 bytes_written = %v, want 14", out["bytes_written"])
	}
	if int(out["size_total"].(float64)) != 14 {
		t.Errorf("chunk 1 size_total = %v, want 14", out["size_total"])
	}

	// Second chunk appended: size_total is the cumulative size, but
	// bytes_written is just this chunk.
	out2, res2 := callJSON(t, tools.writeFile, map[string]any{
		"path":    "doc.html",
		"content": "<h2>part2</h2>",
		"mode":    "append",
	})
	if res2.IsError {
		t.Fatalf("write chunk 2 IsError: %v", out2)
	}
	if int(out2["bytes_written"].(float64)) != 14 {
		t.Errorf("chunk 2 bytes_written = %v, want 14", out2["bytes_written"])
	}
	if int(out2["size_total"].(float64)) != 28 {
		t.Errorf("chunk 2 size_total = %v, want 28 (cumulative)", out2["size_total"])
	}

	// Both chunks are durable on disk in order.
	rd, _ := callJSON(t, tools.readFile, map[string]any{"path": "doc.html"})
	if rd["content"] != "<h1>part1</h1><h2>part2</h2>" {
		t.Errorf("appended content = %q", rd["content"])
	}

	// A default (non-append) write truncates: size_total resets.
	out3, _ := callJSON(t, tools.writeFile, map[string]any{
		"path":    "doc.html",
		"content": "fresh",
	})
	if int(out3["size_total"].(float64)) != 5 {
		t.Errorf("truncate size_total = %v, want 5", out3["size_total"])
	}
}

func TestTools_ReadFile_NotFound(t *testing.T) {
	tools, _ := newTools(t)
	out, res := callJSON(t, tools.readFile, map[string]any{"path": "missing.txt"})
	if !res.IsError {
		t.Fatal("expected IsError")
	}
	if out["code"] != "not_found" {
		t.Errorf("code = %v", out["code"])
	}
}

// TestTools_PathExpandsEnv verifies a file-tool `path` arg that
// embeds an environment variable ($SESSION_DIR) is expanded — weak
// models routinely pass `$SESSION_DIR/out.html` as a path, which the
// shell would expand for bash.run but the file tools must handle
// themselves.
func TestTools_PathExpandsEnv(t *testing.T) {
	tools, _ := newTools(t)
	cwd, _ := os.Getwd()
	t.Setenv("SESSION_DIR", cwd)

	if _, res := callJSON(t, tools.writeFile, map[string]any{
		"path":    "$SESSION_DIR/env.txt",
		"content": "hi",
	}); res.IsError {
		t.Fatalf("write via $SESSION_DIR path errored")
	}
	if _, err := os.Stat(filepath.Join(cwd, "env.txt")); err != nil {
		t.Fatalf("file not written to expanded path: %v", err)
	}
	out, res := callJSON(t, tools.readFile, map[string]any{"path": "$SESSION_DIR/env.txt"})
	if res.IsError {
		t.Fatalf("read via $SESSION_DIR path errored: %v", out)
	}
	if out["content"] != "hi" {
		t.Errorf("content = %v, want hi", out["content"])
	}
}

func TestTools_ListDir(t *testing.T) {
	tools, _ := newTools(t)
	cwd, _ := os.Getwd()
	for _, n := range []string{"a.txt", "b.txt", "c.csv"} {
		_ = os.WriteFile(filepath.Join(cwd, n), []byte(n), 0o644)
	}
	out, res := callJSON(t, tools.listDir, map[string]any{"path": "."})
	if res.IsError {
		t.Fatalf("IsError: %v", out)
	}
	entries := out["entries"].([]any)
	if len(entries) != 3 {
		t.Errorf("entries = %d", len(entries))
	}
	out2, _ := callJSON(t, tools.listDir, map[string]any{"path": ".", "glob": "*.csv"})
	if entries := out2["entries"].([]any); len(entries) != 1 {
		t.Errorf("glob filter: entries = %d", len(entries))
	}
}

func TestTools_Run_HappyPath(t *testing.T) {
	tools, _ := newTools(t)
	out, res := callJSON(t, tools.run, map[string]any{
		"cmd":  "echo",
		"args": []any{"hello"},
	})
	if res.IsError {
		t.Fatalf("IsError: %v", out)
	}
	if int(out["exit_code"].(float64)) != 0 {
		t.Errorf("exit_code = %v", out["exit_code"])
	}
	if !strings.Contains(out["stdout"].(string), "hello") {
		t.Errorf("stdout = %v", out["stdout"])
	}
}

func TestTools_Run_OutputTruncated(t *testing.T) {
	tools, _ := newTools(t)
	tools.Limits.OutputMaxBytes = 16
	out, _ := callJSON(t, tools.run, map[string]any{
		"cmd":   "sh",
		"args":  []any{"-c", "yes hello | head -c 100"},
		"shell": false,
	})
	if !out["truncated"].(bool) {
		t.Errorf("truncated = false, want true")
	}
}

func TestTools_Run_Timeout(t *testing.T) {
	tools, _ := newTools(t)
	out, res := callJSON(t, tools.run, map[string]any{
		"cmd":        "sleep",
		"args":       []any{"5"},
		"timeout_ms": 200,
	})
	if !res.IsError {
		t.Fatalf("expected IsError, got %v", out)
	}
	if out["code"] != "timeout" {
		t.Errorf("code = %v", out["code"])
	}
}

func TestTools_Shell_RunsViaSh(t *testing.T) {
	tools, _ := newTools(t)
	out, res := callJSON(t, tools.shell, map[string]any{
		"cmd": "echo $((1+2))",
	})
	if res.IsError {
		t.Fatalf("IsError: %v", out)
	}
	if !strings.Contains(out["stdout"].(string), "3") {
		t.Errorf("stdout = %v", out["stdout"])
	}
}

func TestTools_Sed_InPlace(t *testing.T) {
	tools, _ := newTools(t)
	cwd, _ := os.Getwd()
	path := filepath.Join(cwd, "f.txt")
	if err := os.WriteFile(path, []byte("foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, res := callJSON(t, tools.sed, map[string]any{
		"path":   "f.txt",
		"script": "s/foo/bar/",
	})
	if res.IsError {
		t.Fatalf("IsError")
	}
	got, _ := os.ReadFile(path)
	if string(got) != "bar\n" {
		t.Errorf("after sed = %q", got)
	}
}

func TestTools_ArgValidation_MissingPath(t *testing.T) {
	tools, _ := newTools(t)
	out, res := callJSON(t, tools.readFile, map[string]any{})
	if !res.IsError {
		t.Fatal("expected IsError")
	}
	if out["code"] != "arg_validation" {
		t.Errorf("code = %v", out["code"])
	}
}


func TestErrCode_KnownClasses(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{ErrPathEscape, "path_escape"},
		{fmt.Errorf("wrap: %w", ErrPathEscape), "path_escape"},
		{errors.New("random"), "io"},
	}
	for _, c := range cases {
		if got := errCode(c.err); got != c.want {
			t.Errorf("errCode(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}
