package skill

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"testing"
	"testing/fstest"
)

// TestTarGzBundle_RoundTripHash proves the wire invariant the marketplace
// relies on: tarring a bundle then re-hashing the extracted tree yields the
// SAME canonical BundleHash — and dotfiles are excluded from both, so a
// sentinel/ledger in the source never changes the archive.
func TestTarGzBundle_RoundTripHash(t *testing.T) {
	src := fstest.MapFS{
		"SKILL.md":        {Data: []byte("name: demo\n")},
		"scripts/run.py":  {Data: []byte("print(1)\n")},
		"references/a.md": {Data: []byte("ref")},
		".hugen-checksum": {Data: []byte("sha256:whatever")}, // dotfile → excluded
		".git/config":     {Data: []byte("[core]")},          // dot-dir → excluded
	}
	srcHash, err := BundleHash(src)
	if err != nil {
		t.Fatalf("source hash: %v", err)
	}

	tarball, err := TarGzBundle(src)
	if err != nil {
		t.Fatalf("tar: %v", err)
	}

	extracted := extractTarGzToMap(t, tarball)
	// The dotfile + dot-dir must NOT be in the archive.
	if _, ok := extracted[".hugen-checksum"]; ok {
		t.Error("archive included a dotfile")
	}
	for name := range extracted {
		if name == ".git/config" {
			t.Error("archive included a dot-directory file")
		}
	}
	if _, ok := extracted["SKILL.md"]; !ok {
		t.Error("archive missing SKILL.md")
	}

	gotHash, err := BundleHash(extracted)
	if err != nil {
		t.Fatalf("extracted hash: %v", err)
	}
	if gotHash != srcHash {
		t.Errorf("round-trip hash mismatch: source %s, extracted %s", srcHash, gotHash)
	}
}

func extractTarGzToMap(t *testing.T, data []byte) fstest.MapFS {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	out := fstest.MapFS{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read entry: %v", err)
		}
		out[hdr.Name] = &fstest.MapFile{Data: b}
	}
	return out
}
