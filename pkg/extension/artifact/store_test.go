package artifact

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newStore(t *testing.T, maxTotal, maxSession int64) (*Store, string) {
	t.Helper()
	base := t.TempDir()
	return NewStore(base, "agent-x", maxTotal, maxSession, discardLog(t)), base
}

// src writes a scratch source file (outside the store) and returns its
// absolute path — stands in for a workspace file being published.
func src(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStore_RegisterAndList(t *testing.T) {
	s, _ := newStore(t, 0, 0)
	if _, err := s.Register("ses-root", src(t, "b.txt", "bbbb"), "", "", false); err != nil {
		t.Fatalf("register b: %v", err)
	}
	if _, err := s.Register("ses-root", src(t, "a.txt", "aa"), "", "", false); err != nil {
		t.Fatalf("register a: %v", err)
	}
	refs, err := s.List("ses-root")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("want 2 refs, got %d", len(refs))
	}
	// name-sorted
	if refs[0].ID != "a.txt" || refs[1].ID != "b.txt" {
		t.Errorf("unsorted: %q %q", refs[0].ID, refs[1].ID)
	}
	if refs[0].Size != 2 || refs[1].Size != 4 {
		t.Errorf("sizes: %d %d", refs[0].Size, refs[1].Size)
	}
	if refs[0].Name != "a.txt" || refs[0].CreatedAt.IsZero() {
		t.Errorf("ref metadata incomplete: %+v", refs[0])
	}
	if !strings.HasPrefix(refs[0].MIME, "text/plain") {
		t.Errorf("mime = %q want text/plain*", refs[0].MIME)
	}
}

func TestStore_ListEmptyScope(t *testing.T) {
	s, _ := newStore(t, 0, 0)
	refs, err := s.List("never-published")
	if err != nil {
		t.Fatalf("list absent scope: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("want empty, got %d", len(refs))
	}
}

func TestStore_RegisterCollision(t *testing.T) {
	s, _ := newStore(t, 0, 0)
	if _, err := s.Register("r", src(t, "x.txt", "one"), "", "", false); err != nil {
		t.Fatalf("first: %v", err)
	}
	_, err := s.Register("r", src(t, "x.txt", "two"), "", "", false)
	if !errors.Is(err, ErrExists) {
		t.Fatalf("want ErrExists, got %v", err)
	}
	// overwrite replaces in place
	ref, err := s.Register("r", src(t, "x.txt", "longer-second"), "", "", true)
	if err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if ref.Size != int64(len("longer-second")) {
		t.Errorf("overwrite size = %d", ref.Size)
	}
}

func TestStore_RegisterDefaultAndSanitizedName(t *testing.T) {
	s, _ := newStore(t, 0, 0)
	// default name = source basename
	ref, err := s.Register("r", src(t, "Report Final.txt", "x"), "", "", false)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if ref.ID != "Report-Final.txt" {
		t.Errorf("sanitized id = %q want Report-Final.txt", ref.ID)
	}
	// explicit name overrides + is sanitized
	ref2, err := s.Register("r", src(t, "raw.bin", "y"), "my report/v2.md", "", false)
	if err != nil {
		t.Fatalf("register named: %v", err)
	}
	if ref2.ID != "v2.md" { // path part dropped by Base, space→-
		t.Errorf("named id = %q want v2.md", ref2.ID)
	}
}

func TestStore_RegisterBadName(t *testing.T) {
	s, _ := newStore(t, 0, 0)
	if _, err := s.Register("r", src(t, "ok.txt", "x"), "...", "", false); !errors.Is(err, ErrBadName) {
		t.Fatalf("want ErrBadName for name=..., got %v", err)
	}
}

func TestStore_RegisterMissingSource(t *testing.T) {
	s, _ := newStore(t, 0, 0)
	_, err := s.Register("r", filepath.Join(t.TempDir(), "ghost.txt"), "", "", false)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestStore_QuotaSession(t *testing.T) {
	s, _ := newStore(t, 0, 10) // 10-byte per-session cap
	if _, err := s.Register("r", src(t, "a.txt", "12345"), "", "", false); err != nil {
		t.Fatalf("first 5 bytes: %v", err)
	}
	// +6 bytes → 11 > 10 → reject
	if _, err := s.Register("r", src(t, "b.txt", "123456"), "", "", false); !errors.Is(err, ErrQuota) {
		t.Fatalf("want ErrQuota, got %v", err)
	}
	// overwrite the first frees its 5 bytes, so a 6-byte replace fits
	if _, err := s.Register("r", src(t, "a.txt", "123456"), "a.txt", "", true); err != nil {
		t.Fatalf("overwrite within quota: %v", err)
	}
}

func TestStore_CopyAndPath(t *testing.T) {
	s, _ := newStore(t, 0, 0)
	if _, err := s.Register("r", src(t, "doc.txt", "hello"), "", "", false); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "ws", "out.txt")
	got, err := s.Copy("r", "doc.txt", dest)
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	b, _ := os.ReadFile(got)
	if string(b) != "hello" {
		t.Errorf("copied bytes = %q", b)
	}
	// Path resolves an existing id; rejects traversal + missing
	if _, err := s.Path("r", "doc.txt"); err != nil {
		t.Errorf("path existing: %v", err)
	}
	for _, bad := range []string{"../x", "..", "a/b", "ghost.txt"} {
		if _, err := s.Path("r", bad); !errors.Is(err, ErrNotFound) {
			t.Errorf("path %q: want ErrNotFound, got %v", bad, err)
		}
	}
}

func TestStore_Delete(t *testing.T) {
	s, _ := newStore(t, 0, 0)
	if _, err := s.Register("r", src(t, "z.txt", "x"), "", "", false); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("r", "z.txt"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.Delete("r", "z.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("re-delete: want ErrNotFound, got %v", err)
	}
}

func TestStore_ReapRoot(t *testing.T) {
	s, _ := newStore(t, 0, 0)
	if _, err := s.Register("r", src(t, "a.txt", "x"), "", "", false); err != nil {
		t.Fatal(err)
	}
	if err := s.ReapRoot("r"); err != nil {
		t.Fatalf("reap: %v", err)
	}
	refs, _ := s.List("r")
	if len(refs) != 0 {
		t.Errorf("scope survived reap: %d", len(refs))
	}
	// reaping an absent root is a no-op
	if err := s.ReapRoot("never"); err != nil {
		t.Errorf("reap absent: %v", err)
	}
}

func TestStore_ReapIdle(t *testing.T) {
	s, base := newStore(t, 0, 0)
	if _, err := s.Register("old", src(t, "a.txt", "x"), "", "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Register("fresh", src(t, "b.txt", "x"), "", "", false); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	// Age "old"'s file 10 days back.
	oldFile := filepath.Join(base, "agent-x", "old", "a.txt")
	past := now.Add(-10 * 24 * time.Hour)
	if err := os.Chtimes(oldFile, past, past); err != nil {
		t.Fatal(err)
	}
	// A live root is never reaped even when its files are old.
	liveOld := func(rootID string) bool { return rootID == "old" }
	if got, _ := s.ReapIdle(7*24*time.Hour, now, liveOld); got != 0 {
		t.Errorf("live root reaped: %d", got)
	}
	if refs, _ := s.List("old"); len(refs) != 1 {
		t.Errorf("live old scope was reaped")
	}
	// Not live → reaped; fresh survives.
	n, err := s.ReapIdle(7*24*time.Hour, now, nil)
	if err != nil {
		t.Fatalf("reapIdle: %v", err)
	}
	if n != 1 {
		t.Errorf("reaped %d, want 1", n)
	}
	if refs, _ := s.List("old"); len(refs) != 0 {
		t.Errorf("old scope survived")
	}
	if refs, _ := s.List("fresh"); len(refs) != 1 {
		t.Errorf("fresh scope reaped")
	}
	// ttl<=0 disables
	if got, _ := s.ReapIdle(0, now, nil); got != 0 {
		t.Errorf("ttl=0 reaped %d", got)
	}
}

func TestSanitizeID(t *testing.T) {
	cases := map[string]string{
		"report.md":                      "report.md",
		"Road Report.md":                 "Road-Report.md",
		"a/b/c.txt":                      "c.txt", // Base drops dirs
		"../../etc/passwd":               "passwd",
		"  spaced  .txt  ":               "spaced-.txt", // internal+edge runs collapse/trim
		".hidden":                        "hidden",
		"":                               "",
		"...":                            "",
		"naïve-café.md":                  "na-ve-caf-.md",
		strings.Repeat("x", 200) + ".md": strings.Repeat("x", 128),
	}
	for in, want := range cases {
		if got := sanitizeID(in); got != want {
			t.Errorf("sanitizeID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSniffMIME(t *testing.T) {
	dir := t.TempDir()
	png := filepath.Join(dir, "img.png")
	// PNG magic header
	if err := os.WriteFile(png, []byte("\x89PNG\r\n\x1a\n....."), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := sniffMIME(png, "img.png"); got != "image/png" {
		t.Errorf("png by ext = %q", got)
	}
	// No extension → content sniff catches the PNG magic.
	noext := filepath.Join(dir, "blob")
	if err := os.WriteFile(noext, []byte("\x89PNG\r\n\x1a\n....."), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := sniffMIME(noext, "blob"); got != "image/png" {
		t.Errorf("png by content = %q", got)
	}
	// Plain text, no extension.
	txt := filepath.Join(dir, "notes")
	if err := os.WriteFile(txt, []byte("just words here"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := sniffMIME(txt, "notes"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("text content = %q want text/plain*", got)
	}
}
