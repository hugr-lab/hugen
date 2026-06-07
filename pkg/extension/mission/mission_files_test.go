package mission

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// scaffoldDataModel mirrors the analyst research/data-model.md skeleton
// shape: headings, a blockquote, an HTML comment, and a placeholder
// bullet still carrying a `<token>`. After the real-content filter it
// has ZERO real lines, so collectMissionFiles must treat it as an
// unfilled scaffold and exclude it.
const scaffoldDataModel = `# Data model

> Filled in by the researcher. Workers read this FIRST.

## Sources & modules

<!-- which Hugr source backs each domain. one line each. -->

### <type_name>

- **module**: <dotted.module.path>
`

// filledResearch carries two real prose lines beyond the scaffold, so
// it must be listed.
const filledResearch = `# Research

## Scope decisions

We analyse EMEA orders for 2023, EUR amounts only.
Soft-delete via orders.deleted_at must be filtered out.
`

func TestCollectMissionFiles_ListsOnlyFilled(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	// Excluded: an unfilled markdown scaffold (the research `before` hook
	// seeds these — listing it would send a worker chasing a stub).
	write("research/data-model.md", scaffoldDataModel)
	// Included: a filled research artifact (≥2 real-content lines).
	write("research/research.md", filledResearch)
	// Included: a data file — non-markdown, never a scaffold, judged by
	// size>0 alone.
	write("data/orders.parquet", "PAR1\x00\x00binary-ish\x00rows")
	// Included: a filled plain-text note.
	write("notes.txt", "two real lines\nsecond real line\n")
	// Excluded: an empty file (size 0).
	write("empty.csv", "")
	// Excluded: hidden file + hidden dir subtree.
	write(".secret.md", filledResearch)
	write(".git/config", "[core]\n\trepositoryformatversion = 0\n")

	got := collectMissionFiles(dir)
	gotPaths := make([]string, 0, len(got))
	for _, f := range got {
		gotPaths = append(gotPaths, f.Path)
	}
	sort.Strings(gotPaths)

	want := []string{"data/orders.parquet", "notes.txt", "research/research.md"}
	if len(gotPaths) != len(want) {
		t.Fatalf("file count = %d %v, want %d %v", len(gotPaths), gotPaths, len(want), want)
	}
	for i := range want {
		if gotPaths[i] != want[i] {
			t.Errorf("path[%d] = %q, want %q (full: %v)", i, gotPaths[i], want[i], gotPaths)
		}
	}
	// Data file must carry a human size (size>0).
	for _, f := range got {
		if f.Path == "data/orders.parquet" && f.Size == "" {
			t.Errorf("data file should report a non-empty Size; got %+v", f)
		}
	}
}

func TestCollectMissionFiles_EmptyDir(t *testing.T) {
	if got := collectMissionFiles(t.TempDir()); got != nil {
		t.Errorf("empty dir → nil index, got %v", got)
	}
	if got := collectMissionFiles(""); got != nil {
		t.Errorf("blank dir → nil index, got %v", got)
	}
}

func TestRealContentLines(t *testing.T) {
	if n := realContentLines(scaffoldDataModel); n >= 2 {
		t.Errorf("scaffold should have <2 real lines, got %d", n)
	}
	if n := realContentLines(filledResearch); n < 2 {
		t.Errorf("filled file should have ≥2 real lines, got %d", n)
	}
}

func TestHumanSize(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, ""},
		{-1, ""},
		{512, "512 B"},
		{1536, "1.5 KB"},
		{5 * 1024 * 1024, "5.0 MB"},
	}
	for _, c := range cases {
		if got := humanSize(c.in); got != c.want {
			t.Errorf("humanSize(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}
