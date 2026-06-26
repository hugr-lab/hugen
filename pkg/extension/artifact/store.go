// Package artifact implements the durable, user-facing artifact
// store (design 007 / Phase 8) and the session extension that exposes
// it to the agent as four tools (list / copy / publish / delete).
//
// Artifacts are plain files under <base>/<agent>/<root_id>/ — the
// FOLDER IS THE REGISTRY (no DB): List reads the directory and derives
// each ref's metadata (name = filename, type = sniff, size = stat,
// created_at = mtime). The id is the (sanitized, path/URL-safe)
// filename; holding it is the access capability, scoped to the root
// conversation. Bytes never travel on a frame — the store hands out
// on-disk PATHS; adapters move bytes out-of-band (design 007 §4).
package artifact

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// Store sentinels — errors.Is-comparable.
var (
	// ErrExists — a publish hit an existing name without overwrite:true.
	ErrExists = errors.New("artifact: already exists")
	// ErrNotFound — no artifact with the given id in the scope.
	ErrNotFound = errors.New("artifact: not found")
	// ErrQuota — the publish would exceed a configured size quota.
	ErrQuota = errors.New("artifact: quota exceeded")
	// ErrBadName — the supplied name sanitizes to nothing usable.
	ErrBadName = errors.New("artifact: invalid name")
)

// Store is the agent-level, folder-backed artifact store. One
// instance per agent; sessions scope into it by root_id. Methods are
// safe for sequential per-session use; the filesystem is the only
// shared state (publishes are infrequent — quota sums walk the tree
// each call rather than caching).
type Store struct {
	base       string // artifacts.dir
	agentID    string
	maxTotal   int64 // whole store, bytes; 0 = unlimited
	maxSession int64 // per root_id, bytes; 0 = unlimited
	log        *slog.Logger
}

// NewStore constructs the store rooted at base for agentID. base is
// created lazily on first publish. maxTotal / maxSession of 0 disable
// the respective quota.
func NewStore(base, agentID string, maxTotal, maxSession int64, log *slog.Logger) *Store {
	if log == nil {
		log = slog.Default()
	}
	return &Store{base: base, agentID: agentID, maxTotal: maxTotal, maxSession: maxSession, log: log}
}

// sessionDir is the on-disk folder for a root conversation's
// artifacts. agentID + rootID are runtime-generated ids (safe path
// segments); a defensive Base guards against a stray separator.
func (s *Store) sessionDir(rootID string) string {
	return filepath.Join(s.base, filepath.Base(s.agentID), filepath.Base(rootID))
}

// List returns the scope's artifacts, name-sorted for determinism.
// An absent scope dir is not an error — it lists empty.
func (s *Store) List(rootID string) ([]protocol.ArtifactRef, error) {
	dir := s.sessionDir(rootID)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	refs := make([]protocol.ArtifactRef, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ref, rerr := s.refFor(dir, e.Name())
		if rerr != nil {
			s.log.Warn("artifact: stat failed, skipping", "name", e.Name(), "err", rerr)
			continue
		}
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].ID < refs[j].ID })
	return refs, nil
}

// refFor builds the metadata ref for one file (name = id = filename).
func (s *Store) refFor(dir, name string) (protocol.ArtifactRef, error) {
	full := filepath.Join(dir, name)
	fi, err := os.Stat(full)
	if err != nil {
		return protocol.ArtifactRef{}, err
	}
	return protocol.ArtifactRef{
		ID:        name,
		Name:      name,
		MIME:      sniffMIME(full, name),
		Size:      fi.Size(),
		CreatedAt: fi.ModTime().UTC(),
	}, nil
}

// Register copies srcPath into the scope as a sanitized artifact and
// returns its ref. name defaults to srcPath's base. overwrite=false
// against an existing id returns ErrExists; a non-zero quota that the
// new bytes would exceed returns ErrQuota (nothing is written).
func (s *Store) Register(rootID, srcPath, name, _ string, overwrite bool) (protocol.ArtifactRef, error) {
	if name == "" {
		name = filepath.Base(srcPath)
	}
	id := sanitizeID(name)
	if id == "" {
		return protocol.ArtifactRef{}, ErrBadName
	}
	sfi, err := os.Stat(srcPath)
	if err != nil {
		if os.IsNotExist(err) {
			return protocol.ArtifactRef{}, fmt.Errorf("%w: source %s", ErrNotFound, srcPath)
		}
		return protocol.ArtifactRef{}, err
	}
	if sfi.IsDir() {
		return protocol.ArtifactRef{}, fmt.Errorf("source %s is a directory", srcPath)
	}

	dir := s.sessionDir(rootID)
	dest := filepath.Join(dir, id)
	existing := int64(0)
	if dfi, derr := os.Stat(dest); derr == nil {
		if !overwrite {
			return protocol.ArtifactRef{}, fmt.Errorf("%w: %s", ErrExists, id)
		}
		existing = dfi.Size()
	}
	if err := s.checkQuota(rootID, sfi.Size(), existing); err != nil {
		return protocol.ArtifactRef{}, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return protocol.ArtifactRef{}, fmt.Errorf("mkdir scope: %w", err)
	}
	if err := copyFile(srcPath, dest); err != nil {
		return protocol.ArtifactRef{}, err
	}
	return s.refFor(dir, id)
}

// Copy duplicates the artifact's bytes to dest (an absolute path the
// caller already confined). Returns dest on success.
func (s *Store) Copy(rootID, id, dest string) (string, error) {
	src, err := s.Path(rootID, id)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", fmt.Errorf("mkdir dest: %w", err)
	}
	if err := copyFile(src, dest); err != nil {
		return "", err
	}
	return dest, nil
}

// Path resolves an artifact id to its on-disk file, confined to the
// scope (a traversal-y id yields ErrNotFound, never an escape). Used
// by Copy + adapters fetching bytes out-of-band + multimodal reads.
func (s *Store) Path(rootID, id string) (string, error) {
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, `/\`) {
		return "", ErrNotFound
	}
	full := filepath.Join(s.sessionDir(rootID), id)
	if _, err := os.Stat(full); err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return "", err
	}
	return full, nil
}

// Delete removes an artifact from the scope. Missing id → ErrNotFound.
func (s *Store) Delete(rootID, id string) error {
	full, err := s.Path(rootID, id)
	if err != nil {
		return err
	}
	return os.Remove(full)
}

// ReapRoot removes a whole root conversation's artifact folder. Called
// on ROOT-session close (design 007 §7). A never-published root has no
// dir; RemoveAll on an absent path is a no-op.
func (s *Store) ReapRoot(rootID string) error {
	return os.RemoveAll(s.sessionDir(rootID))
}

// ReapIdle removes every root scope whose newest file is older than
// ttl, returning the count reaped. ttl<=0 is a no-op. Catches roots
// that never cleanly closed (crash, abandon); the periodic sweep
// drives it (design 007 §7).
func (s *Store) ReapIdle(ttl time.Duration, now time.Time) (int, error) {
	if ttl <= 0 {
		return 0, nil
	}
	agentDir := filepath.Join(s.base, filepath.Base(s.agentID))
	roots, err := os.ReadDir(agentDir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	cutoff := now.Add(-ttl)
	reaped := 0
	for _, r := range roots {
		if !r.IsDir() {
			continue
		}
		newest, ok := newestMTime(filepath.Join(agentDir, r.Name()))
		if !ok {
			continue // empty / unreadable — leave it for the next sweep
		}
		if newest.Before(cutoff) {
			if rerr := os.RemoveAll(filepath.Join(agentDir, r.Name())); rerr != nil {
				s.log.Warn("artifact: idle reap failed", "root", r.Name(), "err", rerr)
				continue
			}
			reaped++
		}
	}
	return reaped, nil
}

// checkQuota rejects a publish that would push the session or whole
// store over a configured cap. existing is the size freed by an
// overwrite (0 for a new artifact).
func (s *Store) checkQuota(rootID string, newSize, existing int64) error {
	if s.maxSession > 0 {
		used, err := dirSize(s.sessionDir(rootID))
		if err != nil {
			return err
		}
		if used-existing+newSize > s.maxSession {
			return fmt.Errorf("%w: session %d + %d > %d bytes", ErrQuota, used-existing, newSize, s.maxSession)
		}
	}
	if s.maxTotal > 0 {
		used, err := dirSize(filepath.Join(s.base, filepath.Base(s.agentID)))
		if err != nil {
			return err
		}
		if used-existing+newSize > s.maxTotal {
			return fmt.Errorf("%w: total %d + %d > %d bytes", ErrQuota, used-existing, newSize, s.maxTotal)
		}
	}
	return nil
}

// ---- helpers ----

// idUnsafe matches any run of characters outside the path/URL-safe set
// kept in a sanitized artifact id.
var idUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// sanitizeID turns a user/agent-supplied name into a readable,
// path/URL-safe filename: drop any directory part, collapse unsafe
// runs to '-', trim separators, cap length, never empty. Two names
// that sanitize alike collide in the folder — resolved by the publish
// overwrite guard, never a silent escape.
func sanitizeID(name string) string {
	name = filepath.Base(name)
	name = idUnsafe.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-.")
	if len(name) > 128 {
		name = strings.Trim(name[:128], "-.")
	}
	if name == "" || name == "." || name == ".." {
		return ""
	}
	return name
}

// sniffMIME derives a content type: known file extension first (cheap,
// accurate), else a 512-byte content sniff, else octet-stream.
func sniffMIME(path, name string) string {
	if ext := filepath.Ext(name); ext != "" {
		if t := mime.TypeByExtension(ext); t != "" {
			return t
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return "application/octet-stream"
	}
	defer f.Close()
	buf := make([]byte, 512)
	n, _ := f.Read(buf)
	return http.DetectContentType(buf[:n])
}

// copyFile copies src to dest (truncating an existing dest), 0o644.
func copyFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create dest: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("copy bytes: %w", err)
	}
	return out.Close()
}

// dirSize sums the regular-file sizes directly under dir (artifacts
// are flat per scope; the agent-total walk sums one level of scope
// subdirs via recursion). Absent dir → 0.
func dirSize(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		fi, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		total += fi.Size()
		return nil
	})
	return total, err
}

// newestMTime returns the most-recent file mtime under dir, ok=false
// when dir holds no readable file.
func newestMTime(dir string) (time.Time, bool) {
	var newest time.Time
	ok := false
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if fi, ierr := d.Info(); ierr == nil {
			if !ok || fi.ModTime().After(newest) {
				newest = fi.ModTime()
				ok = true
			}
		}
		return nil
	})
	return newest, ok
}
