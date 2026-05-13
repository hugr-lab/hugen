package prompts_test

import (
	"io/fs"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/hugr-lab/hugen/assets"
	"github.com/hugr-lab/hugen/pkg/prompts"
)

// promptsFS returns the production embedded source — the
// assets.PromptsFS scoped to the prompts/ sub-tree.
func promptsFS(t *testing.T) fs.FS {
	t.Helper()
	sub, err := fs.Sub(assets.PromptsFS, "prompts")
	if err != nil {
		t.Fatalf("fs.Sub: %v", err)
	}
	return sub
}

// TestRender_AllBundled exercises every α.0-extracted template
// against representative data: each must load, parse, execute
// without error, and produce non-empty output. Golden-shape
// assertions live in TestRender_GoldenSamples below for the
// templates whose exact wording is load-bearing.
func TestRender_AllBundled(t *testing.T) {
	r := prompts.NewRenderer(promptsFS(t), nil)
	cases := []struct {
		name string
		data any
	}{
		{"interrupts/stuck_repeated_tool", map[string]any{"N": 5}},
		{"interrupts/stuck_tight_density", map[string]any{"M": 9, "Window": "30s"}},
		{"interrupts/soft_warning_root", map[string]any{"Turns": 25}},
		{"interrupts/soft_warning_subagent", map[string]any{
			"Turns": 25, "Task": "analyze logs", "CanSpawnDeeper": true,
		}},
		{"system/spawned_note", map[string]any{
			"ChildID": "sess-abc", "Role": "analyst", "Depth": 1,
		}},
		{"system/subagent_result_render", map[string]any{
			"ChildID": "sess-abc", "Reason": "completed", "Turns": 4,
			"Body": "done.",
		}},
		{"skill/available_missions", map[string]any{
			"Missions": []map[string]any{
				{"Name": "analyst", "Summary": "data exploration"},
				{"Name": "_general", "Summary": "fallback dispatcher"},
			},
		}},
		{"skill/catalogue", map[string]any{
			"Skills": []map[string]any{
				{"Name": "analyst", "Description": "explore data", "Loaded": true},
				{"Name": "duckdb-data", "Description": "duckdb access", "Loaded": false},
			},
		}},
		{"notepad/recommended_tags", map[string]any{
			"Tags": []map[string]any{
				{"Name": "schema", "Hint": "structural facts"},
				{"Name": "anomaly"},
			},
		}},
		{"notepad/session_snapshot", map[string]any{
			"Window": "48h",
			"Groups": []map[string]any{
				{"Category": "schema", "Count": 3, "Snippet": "events table",
					"Author": "worker", "Age": "12m", "MoreCount": 2},
			},
			"Truncated": false,
		}},
		{"plan/snapshot_render", map[string]any{
			"CurrentStep": "stage A", "Body": "1. do X\n2. do Y",
		}},
		{"interrupts/follow_up_with_active_subagents", map[string]any{
			"UserMessage": "switch tack",
			"Subagents": []map[string]any{
				{"ID": "sub-1", "Role": "analyst", "Status": "running", "Goal": "explore catalog"},
			},
		}},
		{"interrupts/parent_note_with_active_workers", map[string]any{
			"ParentRole": "mission", "ParentID": "mis-1",
			"Content": "narrow to last 24h only",
			"Workers": []map[string]any{
				{"ID": "wkr-1", "Status": "running", "Goal": "compute deltas"},
			},
		}},
		{"interrupts/parent_note_buffered", map[string]any{
			"ParentRole": "root", "ParentID": "root-1",
			"Content": "drop the dimension column",
		}},
		{"interrupts/async_mission_completed", map[string]any{
			"MissionID":     "mis-7",
			"Goal":          "compute deltas",
			"Status":        "completed",
			"ResultSummary": "120 rows processed",
		}},
		{"inquiry/approval_request_summary", map[string]any{
			"ToolName":    "bash.run",
			"ArgsSummary": "{\"cmd\":\"rm -rf foo\"}",
		}},
		{"inquiry/timeout_notice", map[string]any{
			"Timeout": "1h", "DefaultAction": "deny",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := r.Render(tc.name, tc.data)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if strings.TrimSpace(out) == "" {
				t.Fatalf("Render produced empty output")
			}
		})
	}
}

// TestRender_GoldenSamples pins wording on a representative
// subset of templates. Bundled prose is part of the contract;
// silent drift breaks scenario tuning.
func TestRender_GoldenSamples(t *testing.T) {
	r := prompts.NewRenderer(promptsFS(t), nil)

	// stuck_repeated_tool — the simplest parameter substitution.
	got, err := r.Render("interrupts/stuck_repeated_tool", map[string]any{"N": 5})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := "You've called the same tool with identical arguments 5 times in a row. The loop may be intentional — consider whether changing the approach or pausing to think would help. If you're confident, continue.\n"
	if got != want {
		t.Errorf("stuck_repeated_tool mismatch:\n got: %q\nwant: %q", got, want)
	}

	// soft_warning_subagent with both conditional branches:
	// Task non-empty → " inside task" clause; CanSpawnDeeper=true
	// → spawn_subagent tail.
	got, err = r.Render("interrupts/soft_warning_subagent", map[string]any{
		"Turns": 30, "Task": "audit", "CanSpawnDeeper": true,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	wantSub := `inside task "audit"`
	wantTail := "spawn_subagent"
	if !strings.Contains(got, wantSub) {
		t.Errorf("soft_warning_subagent missing task clause: %q", got)
	}
	if !strings.Contains(got, wantTail) {
		t.Errorf("soft_warning_subagent missing spawn tail: %q", got)
	}

	// Same template with CanSpawnDeeper=false picks the depth-cap
	// branch.
	got, err = r.Render("interrupts/soft_warning_subagent", map[string]any{
		"Turns": 30, "Task": "", "CanSpawnDeeper": false,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(got, "inside task") {
		t.Errorf("soft_warning_subagent leaked task clause when empty: %q", got)
	}
	if !strings.Contains(got, "Sub-sub-agents are not available at this depth.") {
		t.Errorf("soft_warning_subagent missing depth-cap branch: %q", got)
	}
}

// TestRender_MissingTemplate surfaces ENOENT for an unknown
// name. Strict — the call site decides whether to degrade.
func TestRender_MissingTemplate(t *testing.T) {
	r := prompts.NewRenderer(promptsFS(t), nil)
	_, err := r.Render("does/not/exist", nil)
	if err == nil {
		t.Fatalf("expected error for missing template")
	}
	if !strings.Contains(err.Error(), "does/not/exist") {
		t.Errorf("error does not name the missing template: %v", err)
	}
}

// TestRender_CachedAfterFirstLoad verifies the second render
// of the same template does not re-read from the underlying
// filesystem. Uses a counting fstest.MapFS to observe.
func TestRender_CachedAfterFirstLoad(t *testing.T) {
	src := fstest.MapFS{
		"sample.tmpl": &fstest.MapFile{Data: []byte("hi {{.Name}}\n")},
	}
	counter := &readCounter{inner: src}
	r := prompts.NewRenderer(counter, nil)

	for i := 0; i < 5; i++ {
		out, err := r.Render("sample", map[string]any{"Name": "world"})
		if err != nil {
			t.Fatalf("Render %d: %v", i, err)
		}
		if out != "hi world\n" {
			t.Fatalf("Render %d: got %q", i, out)
		}
	}
	if counter.opens != 1 {
		t.Errorf("expected 1 fs open, got %d", counter.opens)
	}
}

// TestRender_ConcurrentSafe exercises the sync.Map cache under
// `go test -race`. 200 goroutines, mixed templates, expecting
// no race and no error.
func TestRender_ConcurrentSafe(t *testing.T) {
	r := prompts.NewRenderer(promptsFS(t), nil)
	names := []string{
		"interrupts/stuck_repeated_tool",
		"interrupts/stuck_tight_density",
		"interrupts/soft_warning_root",
		"system/spawned_note",
	}
	data := map[string]any{
		"N": 1, "M": 2, "Window": "5s", "Turns": 10,
		"ChildID": "x", "Role": "y", "Depth": 0,
	}
	var wg sync.WaitGroup
	wg.Add(200)
	for i := 0; i < 200; i++ {
		go func(i int) {
			defer wg.Done()
			name := names[i%len(names)]
			if _, err := r.Render(name, data); err != nil {
				t.Errorf("Render %s: %v", name, err)
			}
		}(i)
	}
	wg.Wait()
}

// readCounter is an fs.FS wrapper that counts Open calls. Used
// to assert template caching elides repeat reads.
type readCounter struct {
	inner fs.FS
	mu    sync.Mutex
	opens int
}

func (c *readCounter) Open(name string) (fs.File, error) {
	c.mu.Lock()
	c.opens++
	c.mu.Unlock()
	return c.inner.Open(name)
}
