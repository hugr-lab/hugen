package skill

import (
	"strings"
	"testing"
	"testing/fstest"
)

func mustHash(t *testing.T, fsys fstest.MapFS) string {
	t.Helper()
	h, err := BundleHash(fsys)
	if err != nil {
		t.Fatalf("BundleHash: %v", err)
	}
	return h
}

func TestBundleHash_FormatAndStability(t *testing.T) {
	fsys := fstest.MapFS{
		"SKILL.md":            {Data: []byte("# demo\nprose")},
		"scripts/run.py":      {Data: []byte("print('hi')")},
		"references/notes.md": {Data: []byte("ref")},
	}
	got := mustHash(t, fsys)
	if !strings.HasPrefix(got, "sha256:") {
		t.Fatalf("missing sha256: prefix: %q", got)
	}
	if hex := strings.TrimPrefix(got, "sha256:"); len(hex) != 64 {
		t.Fatalf("hex length = %d, want 64 (%q)", len(hex), got)
	}
	// Deterministic: same content, recomputed, must match.
	if again := mustHash(t, fsys); again != got {
		t.Fatalf("non-deterministic: %q vs %q", got, again)
	}
}

func TestBundleHash_DotfilesExcluded(t *testing.T) {
	base := fstest.MapFS{
		"SKILL.md":       {Data: []byte("prose")},
		"scripts/run.py": {Data: []byte("code")},
	}
	withDots := fstest.MapFS{
		"SKILL.md":           {Data: []byte("prose")},
		"scripts/run.py":     {Data: []byte("code")},
		".hugen-checksum":    {Data: []byte("sha256:whatever")},
		".installed.json":    {Data: []byte(`{"x":1}`)},
		".git/config":        {Data: []byte("[core]")},
		".hidden/secret.txt": {Data: []byte("s")},
	}
	if mustHash(t, base) != mustHash(t, withDots) {
		t.Fatal("dotfiles / dot-directories changed the hash — they must be excluded")
	}
}

func TestBundleHash_ScriptContentSensitive(t *testing.T) {
	// The whole point of the canonical hash over bundleHash (SKILL.md-only):
	// a script change MUST move the hash even when SKILL.md is untouched.
	a := fstest.MapFS{
		"SKILL.md":       {Data: []byte("same prose")},
		"scripts/run.py": {Data: []byte("print('v1')")},
	}
	b := fstest.MapFS{
		"SKILL.md":       {Data: []byte("same prose")},
		"scripts/run.py": {Data: []byte("print('v2')")},
	}
	if mustHash(t, a) == mustHash(t, b) {
		t.Fatal("script content change did not move the hash")
	}
}

func TestBundleHash_PathSensitive(t *testing.T) {
	// Moving identical content to a different path changes the hash (relpath
	// is folded in), so a renamed file is a real drift.
	a := fstest.MapFS{"a/x.txt": {Data: []byte("body")}}
	b := fstest.MapFS{"b/x.txt": {Data: []byte("body")}}
	if mustHash(t, a) == mustHash(t, b) {
		t.Fatal("relpath is not folded into the hash")
	}
}

func TestBundleHash_Empty(t *testing.T) {
	// An empty bundle hashes cleanly (no files → sha256 of nothing) — no error.
	got := mustHash(t, fstest.MapFS{})
	if !strings.HasPrefix(got, "sha256:") {
		t.Fatalf("empty bundle: %q", got)
	}
}

// TestBundleHash_NulContentIsInjective guards the length-prefixed framing: a
// bare "<path>\x00<content>\x00" delimiter framing collided when a file's
// CONTENT embedded the delimiter — two files {a:"x", b:"y"} produced the same
// byte stream as one file {a:"x\x00b\x00y"}. Length prefixes must separate them.
func TestBundleHash_NulContentIsInjective(t *testing.T) {
	twoFiles := fstest.MapFS{
		"a": {Data: []byte("x")},
		"b": {Data: []byte("y")},
	}
	oneFile := fstest.MapFS{
		"a": {Data: []byte("x\x00b\x00y")},
	}
	if mustHash(t, twoFiles) == mustHash(t, oneFile) {
		t.Fatal("bundles collided — BundleHash framing is not injective over NUL-containing content")
	}
}
