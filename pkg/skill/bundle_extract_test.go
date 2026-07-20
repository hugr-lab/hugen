package skill

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

// TestExtractTarGz_RoundTrip: TarGzBundle → ExtractTarGz reproduces the tree.
func TestExtractTarGz_RoundTrip(t *testing.T) {
	src := fstest.MapFS{
		"SKILL.md":          {Data: []byte("---\nname: demo\n---\nbody")},
		"references/one.md": {Data: []byte("ref one")},
		"scripts/run.py":    {Data: []byte("print('hi')")},
	}
	tarball, err := TarGzBundle(src)
	if err != nil {
		t.Fatalf("TarGzBundle: %v", err)
	}

	dst := t.TempDir()
	if err := ExtractTarGz(bytes.NewReader(tarball), dst, 1<<20, 64); err != nil {
		t.Fatalf("ExtractTarGz: %v", err)
	}

	for name, entry := range src {
		got, err := os.ReadFile(filepath.Join(dst, name))
		if err != nil {
			t.Errorf("read %q: %v", name, err)
			continue
		}
		if !bytes.Equal(got, entry.Data) {
			t.Errorf("%q: got %q, want %q", name, got, entry.Data)
		}
	}
}

// TestExtractTarGz_RejectsTraversal: a "../escape" entry must be refused, not
// written outside dst.
func TestExtractTarGz_RejectsTraversal(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("pwned")
	if err := tw.WriteHeader(&tar.Header{Name: "../escape.txt", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = gz.Close()

	parent := t.TempDir()
	dst := filepath.Join(parent, "bundle")
	if err := ExtractTarGz(bytes.NewReader(buf.Bytes()), dst, 1<<20, 64); err == nil {
		t.Fatal("expected traversal entry to be rejected")
	}
	if _, err := os.Stat(filepath.Join(parent, "escape.txt")); !os.IsNotExist(err) {
		t.Fatal("traversal wrote a file outside dst")
	}
}

// TestExtractTarGz_ByteCap: the cumulative byte cap is enforced.
func TestExtractTarGz_ByteCap(t *testing.T) {
	src := fstest.MapFS{"big.bin": {Data: bytes.Repeat([]byte("x"), 4096)}}
	tarball, err := TarGzBundle(src)
	if err != nil {
		t.Fatalf("TarGzBundle: %v", err)
	}
	if err := ExtractTarGz(bytes.NewReader(tarball), t.TempDir(), 1024, 64); err == nil {
		t.Fatal("expected byte-cap breach to be rejected")
	}
}
