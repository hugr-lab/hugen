package skill

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ExtractTarGz extracts a gzip-compressed tar (the wire form [TarGzBundle]
// produces) into dst, rejecting any entry that would escape dst — an absolute
// path, a "..", or a symlink/hardlink/device — and enforcing a cumulative byte
// cap and a file-count cap. Only regular files and directories are
// materialised, so a malicious bundle cannot plant a link that writes outside
// the tree. It is the inverse of [TarGzBundle] and the canonical safe-extract
// used by the skills-install path; the byte/file caps are the caller's policy
// (the marketplace uses 32 MiB / 4096 files).
func ExtractTarGz(r io.Reader, dst string, maxBytes int64, maxFiles int) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)

	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("mkdir dst: %w", err)
	}
	root, err := filepath.Abs(dst)
	if err != nil {
		return err
	}

	var total int64
	var files int
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar next: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeDir {
			return fmt.Errorf("unsafe entry type %d in %q", hdr.Typeflag, hdr.Name)
		}
		clean := filepath.Clean(hdr.Name)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe path %q", hdr.Name)
		}
		target := filepath.Join(root, clean)
		// Defense in depth: the joined target must stay within root.
		if target != root && !strings.HasPrefix(target, root+string(filepath.Separator)) {
			return fmt.Errorf("path escapes bundle root: %q", hdr.Name)
		}

		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("mkdir %q: %w", target, err)
			}
			continue
		}
		files++
		if files > maxFiles {
			return fmt.Errorf("too many files (> %d)", maxFiles)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("mkdir parent %q: %w", target, err)
		}
		remaining := maxBytes - total
		if remaining <= 0 {
			return fmt.Errorf("bundle exceeds size cap (%d bytes)", maxBytes)
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return fmt.Errorf("create %q: %w", target, err)
		}
		n, err := io.Copy(f, io.LimitReader(tr, remaining+1))
		_ = f.Close()
		if err != nil {
			return fmt.Errorf("write %q: %w", target, err)
		}
		total += n
		if total > maxBytes {
			return fmt.Errorf("bundle exceeds size cap (%d bytes)", maxBytes)
		}
	}
	return nil
}
