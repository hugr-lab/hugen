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
