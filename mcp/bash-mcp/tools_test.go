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

// TestTools_WriteFile_SizeCap verifies write_file rejects content over
// the per-call byte cap (forcing chunked append) but accepts content
// exactly at the cap.
func TestTools_WriteFile_SizeCap(t *testing.T) {
	tools, _ := newTools(t)

	big := strings.Repeat("x", maxWriteChunkBytes+1)
	out, res := callJSON(t, tools.writeFile, map[string]any{"path": "big.txt", "content": big})
	if !res.IsError {
		t.Fatal("oversized content should be rejected")
	}
	if out["code"] != "arg_validation" {
		t.Errorf("code = %v, want arg_validation", out["code"])
	}

	atCap := strings.Repeat("y", maxWriteChunkBytes)
	out2, res2 := callJSON(t, tools.writeFile, map[string]any{"path": "ok.txt", "content": atCap})
	if res2.IsError {
		t.Fatalf("at-cap write should succeed: %v", out2)
	}
	if int(out2["bytes_written"].(float64)) != maxWriteChunkBytes {
		t.Errorf("bytes_written = %v, want %d", out2["bytes_written"], maxWriteChunkBytes)
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

// withCtxCap returns a Tools with a small ReadContextMaxBytes so the L1
// soft-truncation path is exercised without needing a real large file.
func withCtxCap(t *testing.T, cap int) (*Tools, string) {
	t.Helper()
	tools, _ := newTools(t)
	tools.Limits.ReadContextMaxBytes = cap
	cwd, _ := os.Getwd()
	return tools, cwd
}

// TestReadFile_SoftTruncate covers the L1 read context-cap: a read with
// no explicit length over the cap is truncated to it and carries paging
// offsets, while a read under the cap is returned whole.
func TestReadFile_SoftTruncate(t *testing.T) {
	tools, cwd := withCtxCap(t, 100)
	// 250-byte file written directly (bypasses the write_file cap).
	if err := os.WriteFile(filepath.Join(cwd, "big.txt"), []byte(strings.Repeat("a", 250)), 0o644); err != nil {
		t.Fatal(err)
	}
	out, res := callJSON(t, tools.readFile, map[string]any{"path": "big.txt"})
	if res.IsError {
		t.Fatalf("read IsError: %v", out)
	}
	if int(out["bytes_read"].(float64)) != 100 {
		t.Errorf("bytes_read = %v, want 100", out["bytes_read"])
	}
	if len(out["content"].(string)) != 100 {
		t.Errorf("content len = %d, want 100", len(out["content"].(string)))
	}
	if out["truncated"] != true {
		t.Errorf("truncated = %v, want true", out["truncated"])
	}
	if int(out["next_start"].(float64)) != 100 {
		t.Errorf("next_start = %v, want 100", out["next_start"])
	}
	if int(out["bytes_total"].(float64)) != 250 {
		t.Errorf("bytes_total = %v, want 250", out["bytes_total"])
	}
	// Paging on with next_start continues from where it stopped.
	out2, _ := callJSON(t, tools.readFile, map[string]any{"path": "big.txt", "start": 100})
	if int(out2["bytes_read"].(float64)) != 100 || out2["truncated"] != true {
		t.Errorf("page 2: bytes_read=%v truncated=%v", out2["bytes_read"], out2["truncated"])
	}
	if int(out2["next_start"].(float64)) != 200 || int(out2["bytes_total"].(float64)) != 150 {
		t.Errorf("page 2: next_start=%v bytes_total=%v", out2["next_start"], out2["bytes_total"])
	}
}

// TestReadFile_UnderCap verifies a file smaller than the cap returns
// whole with no truncation marker.
func TestReadFile_UnderCap(t *testing.T) {
	tools, cwd := withCtxCap(t, 100)
	if err := os.WriteFile(filepath.Join(cwd, "small.txt"), []byte("short"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, res := callJSON(t, tools.readFile, map[string]any{"path": "small.txt"})
	if res.IsError {
		t.Fatalf("read IsError: %v", out)
	}
	if out["content"] != "short" {
		t.Errorf("content = %v", out["content"])
	}
	if _, ok := out["truncated"]; ok {
		t.Errorf("truncated marker should be absent for an under-cap read")
	}
	if out["eof"] != true {
		t.Errorf("eof = %v, want true", out["eof"])
	}
}

// TestReadFile_ExplicitLength covers the two explicit-length paths: a
// length at/under the cap is honoured verbatim (deliberate windowing, no
// marker), and a length over the cap is clamped with the marker.
func TestReadFile_ExplicitLength(t *testing.T) {
	tools, cwd := withCtxCap(t, 100)
	if err := os.WriteFile(filepath.Join(cwd, "big.txt"), []byte(strings.Repeat("a", 250)), 0o644); err != nil {
		t.Fatal(err)
	}
	// length 50 (<= cap): honoured, no truncation marker even though
	// more file exists — the caller asked for a specific window.
	out, _ := callJSON(t, tools.readFile, map[string]any{"path": "big.txt", "length": 50})
	if int(out["bytes_read"].(float64)) != 50 {
		t.Errorf("bytes_read = %v, want 50", out["bytes_read"])
	}
	if _, ok := out["truncated"]; ok {
		t.Errorf("explicit length within cap must not set truncated")
	}
	// length 200 (> cap): clamped to the cap with the marker.
	out2, _ := callJSON(t, tools.readFile, map[string]any{"path": "big.txt", "length": 200})
	if int(out2["bytes_read"].(float64)) != 100 {
		t.Errorf("clamped bytes_read = %v, want 100", out2["bytes_read"])
	}
	if out2["truncated"] != true {
		t.Errorf("over-cap length must set truncated")
	}
}

// TestGrep_LiteralWithContext covers literal locate with surrounding
// context lines and line numbers.
func TestGrep_LiteralWithContext(t *testing.T) {
	tools, _ := newTools(t)
	body := "line one\nline two\nTARGET here\nline four\nline five\n"
	callJSON(t, tools.writeFile, map[string]any{"path": "doc.txt", "content": body})

	out, res := callJSON(t, tools.grep, map[string]any{"path": "doc.txt", "pattern": "TARGET", "context": 1})
	if res.IsError {
		t.Fatalf("grep IsError: %v", out)
	}
	if int(out["match_count"].(float64)) != 1 {
		t.Fatalf("match_count = %v, want 1", out["match_count"])
	}
	m := out["matches"].([]any)[0].(map[string]any)
	if int(m["line_no"].(float64)) != 3 {
		t.Errorf("line_no = %v, want 3", m["line_no"])
	}
	if m["text"] != "line two\nTARGET here\nline four" {
		t.Errorf("text = %q", m["text"])
	}
}

// TestGrep_Regex covers regex mode and the max_matches truncation flag.
func TestGrep_Regex(t *testing.T) {
	tools, _ := newTools(t)
	callJSON(t, tools.writeFile, map[string]any{"path": "nums.txt", "content": "a1\nb2\nc3\nd4\n"})

	out, _ := callJSON(t, tools.grep, map[string]any{
		"path": "nums.txt", "pattern": `[0-9]`, "regex": true, "context": 0, "max_matches": 2,
	})
	if int(out["match_count"].(float64)) != 2 {
		t.Errorf("match_count = %v, want 2 (capped)", out["match_count"])
	}
	if out["truncated"] != true {
		t.Errorf("truncated = %v, want true", out["truncated"])
	}
}

// TestGrep_NoMatch returns an empty match set, not an error.
func TestGrep_NoMatch(t *testing.T) {
	tools, _ := newTools(t)
	callJSON(t, tools.writeFile, map[string]any{"path": "doc.txt", "content": "nothing here\n"})
	out, res := callJSON(t, tools.grep, map[string]any{"path": "doc.txt", "pattern": "absent"})
	if res.IsError {
		t.Fatalf("no-match grep should not error: %v", out)
	}
	if int(out["match_count"].(float64)) != 0 {
		t.Errorf("match_count = %v, want 0", out["match_count"])
	}
}

// TestGrep_ClipsLongLine verifies a match on a very long (minified) line
// returns a bounded window centred on the match, not the whole line.
func TestGrep_ClipsLongLine(t *testing.T) {
	tools, cwd := newToolsCwd(t)
	long := strings.Repeat("x", 1000) + "ANCHOR" + strings.Repeat("y", 1000)
	if err := os.WriteFile(filepath.Join(cwd, "min.html"), []byte(long), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ := callJSON(t, tools.grep, map[string]any{"path": "min.html", "pattern": "ANCHOR"})
	m := out["matches"].([]any)[0].(map[string]any)
	text := m["text"].(string)
	if len(text) > maxGrepLineBytes+8 { // +ellipsis bytes
		t.Errorf("clipped text len = %d, want <= ~%d", len(text), maxGrepLineBytes)
	}
	if !strings.Contains(text, "ANCHOR") {
		t.Errorf("clipped window lost the anchor: %q", text)
	}
}

// TestEditFile_UniqueReplace covers the happy path: a unique minimal old
// string is replaced and the file content never enters the args.
func TestEditFile_UniqueReplace(t *testing.T) {
	tools, _ := newTools(t)
	callJSON(t, tools.writeFile, map[string]any{"path": "r.html", "content": "Revenue: $1,234,567.8 total"})

	out, res := callJSON(t, tools.editFile, map[string]any{
		"path": "r.html", "old": "$1,234,567.8", "new": "$1,234,567.89",
	})
	if res.IsError {
		t.Fatalf("edit IsError: %v", out)
	}
	if int(out["replacements"].(float64)) != 1 {
		t.Errorf("replacements = %v, want 1", out["replacements"])
	}
	rd, _ := callJSON(t, tools.readFile, map[string]any{"path": "r.html"})
	if rd["content"] != "Revenue: $1,234,567.89 total" {
		t.Errorf("content = %q", rd["content"])
	}
}

// TestEditFile_NotFound and ambiguous cover the two error codes.
func TestEditFile_NotFound(t *testing.T) {
	tools, _ := newTools(t)
	callJSON(t, tools.writeFile, map[string]any{"path": "r.txt", "content": "hello"})
	out, res := callJSON(t, tools.editFile, map[string]any{"path": "r.txt", "old": "absent", "new": "x"})
	if !res.IsError || out["code"] != "not_found" {
		t.Errorf("want not_found error, got IsError=%v code=%v", res.IsError, out["code"])
	}
}

func TestEditFile_Ambiguous(t *testing.T) {
	tools, _ := newTools(t)
	callJSON(t, tools.writeFile, map[string]any{"path": "r.txt", "content": "x x x"})
	out, res := callJSON(t, tools.editFile, map[string]any{"path": "r.txt", "old": "x", "new": "y"})
	if !res.IsError || out["code"] != "ambiguous" {
		t.Errorf("want ambiguous error, got IsError=%v code=%v", res.IsError, out["code"])
	}
	// replace_all clears the ambiguity.
	out2, res2 := callJSON(t, tools.editFile, map[string]any{"path": "r.txt", "old": "x", "new": "y", "replace_all": true})
	if res2.IsError {
		t.Fatalf("replace_all IsError: %v", out2)
	}
	if int(out2["replacements"].(float64)) != 3 {
		t.Errorf("replacements = %v, want 3", out2["replacements"])
	}
}

// TestEditFile_LineScope resolves an ambiguous match by scoping to a
// single line (from a prior grep).
func TestEditFile_LineScope(t *testing.T) {
	tools, _ := newTools(t)
	callJSON(t, tools.writeFile, map[string]any{"path": "r.txt", "content": "val=1\nval=1\nval=1"})
	line := 2
	out, res := callJSON(t, tools.editFile, map[string]any{
		"path": "r.txt", "old": "val=1", "new": "val=2", "line": line,
	})
	if res.IsError {
		t.Fatalf("line-scoped edit IsError: %v", out)
	}
	rd, _ := callJSON(t, tools.readFile, map[string]any{"path": "r.txt"})
	if rd["content"] != "val=1\nval=2\nval=1" {
		t.Errorf("content = %q", rd["content"])
	}
}

// TestEditFile_IdenticalRejected guards against a no-op old==new edit.
func TestEditFile_IdenticalRejected(t *testing.T) {
	tools, _ := newTools(t)
	callJSON(t, tools.writeFile, map[string]any{"path": "r.txt", "content": "abc"})
	out, res := callJSON(t, tools.editFile, map[string]any{"path": "r.txt", "old": "a", "new": "a"})
	if !res.IsError || out["code"] != "arg_validation" {
		t.Errorf("want arg_validation, got IsError=%v code=%v", res.IsError, out["code"])
	}
}

// newToolsCwd is newTools plus the chdir'd workspace path, for tests
// that drop a file straight onto disk (bypassing the write_file cap).
func newToolsCwd(t *testing.T) (*Tools, string) {
	t.Helper()
	tools, _ := newTools(t)
	cwd, _ := os.Getwd()
	return tools, cwd
}
