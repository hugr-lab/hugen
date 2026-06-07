package mission

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestCollectDeclaredFiles_ListsExistingDeclared(t *testing.T) {
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

	// On disk: the runtime contract + two declared research artifacts +
	// one empty declared file. spec.md is added implicitly by the
	// collector (the runtime authors it).
	write("spec.md", "# Goal\n\n- [ ] ac-1\n")
	write("research/data-model.md", "## Tables\norders, customers\n")
	write("research/queries.md", "query { orders { id } }\n")
	write("research/empty.md", "") // declared but empty → dropped

	refs := []string{
		"research/data-model.md",
		"./research/queries.md",      // ./ prefix normalised → same set, no dup
		"research/queries.md",        // exact dup
		"research/empty.md",          // empty → dropped
		"research/never-written.md",  // declared but absent → dropped
		"../escape.md",               // path escape → rejected
		"/etc/passwd",                // absolute → rejected
		"spec.md",                    // dup of the implicit contract entry
	}

	got := collectDeclaredFiles(dir, refs)
	gotPaths := make([]string, 0, len(got))
	for _, f := range got {
		gotPaths = append(gotPaths, f.Path)
	}
	sort.Strings(gotPaths)

	want := []string{"research/data-model.md", "research/queries.md", "spec.md"}
	if len(gotPaths) != len(want) {
		t.Fatalf("file count = %d %v, want %d %v", len(gotPaths), gotPaths, len(want), want)
	}
	for i := range want {
		if gotPaths[i] != want[i] {
			t.Errorf("path[%d] = %q, want %q (full: %v)", i, gotPaths[i], want[i], gotPaths)
		}
	}
	for _, f := range got {
		if f.Size == "" {
			t.Errorf("listed file %q should report a non-empty Size", f.Path)
		}
	}
}

func TestCollectDeclaredFiles_NilWhenNothingDeclaredExists(t *testing.T) {
	// Empty dir, no refs: spec.md doesn't exist → nil (not a [spec.md] stub).
	if got := collectDeclaredFiles(t.TempDir(), nil); got != nil {
		t.Errorf("empty dir → nil index, got %v", got)
	}
	if got := collectDeclaredFiles("", []string{"research/x.md"}); got != nil {
		t.Errorf("blank dir → nil index, got %v", got)
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
