package skill

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

// BundleHash computes the canonical content hash of a skill bundle: sha256
// over every non-dotfile in fsys, files ordered by relative path, feeding
// "<relpath>\x00<content>\x00" per file. Dotfiles — any path segment
// beginning with "." — are excluded so sentinels like ".hugen-checksum" and
// the install ledger (".installed.json") never fold into the hash.
//
// It is the single drift signal for skill distribution, mandated at four
// points (spec-skills-distribution §2): the marketplace publish row
// (skills.content_hash), the catalog↔local drift compare, the install-ledger
// hash, and the embed-seed sentinel. Result format: "sha256:<hex>".
//
// fsys is a bundle-rooted filesystem: os.DirFS(dir) for an on-disk bundle, or
// fs.Sub(embedFS, "skills/<name>") for an embedded one — the hash is identical
// for identical content regardless of the source, which is what lets the hub
// seed (embed) and the agent reconciler (disk) compare hashes at all.
func BundleHash(fsys fs.FS) (string, error) {
	var paths []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." {
			return nil
		}
		if hasDotSegment(p) {
			// Skip the whole subtree of a dot-directory; skip a dotfile alone.
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("walk bundle: %w", err)
	}

	// fs.WalkDir already visits lexically, but the contract is "sorted by
	// relative path" — assert it so a future backend with a different walk
	// order can't silently change the hash.
	sort.Strings(paths)

	h := sha256.New()
	for _, p := range paths {
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return "", fmt.Errorf("read bundle file %s: %w", p, err)
		}
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(b)
		_, _ = h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// hasDotSegment reports whether any slash-separated segment of p (an fs.FS
// path — always forward-slash) begins with a dot.
func hasDotSegment(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}
