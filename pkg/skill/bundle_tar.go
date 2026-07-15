package skill

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io/fs"
)

// TarGzBundle writes fsys as a gzip-compressed tar, excluding dotfiles and
// dot-directories so the archive covers EXACTLY the files [BundleHash] hashes.
// This is the wire form the skills marketplace moves bundles in: a publisher
// tars its on-disk bundle with this, and the hub re-tars canonically the same
// way, so a downloader's re-hash of the extracted tree equals the catalog
// content_hash. fsys is a bundle-rooted filesystem (os.DirFS(dir) or a
// Skill.FS).
func TarGzBundle(fsys fs.FS) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." {
			return nil
		}
		if hasDotSegment(p) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		content, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		hdr := &tar.Header{
			Name:     p,
			Mode:     0o644,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := tw.Write(content); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
