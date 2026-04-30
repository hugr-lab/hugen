package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// resolveOutPath maps a queried output path onto the local
// filesystem, enforcing the per-session and shared boundaries.
//
// Rules:
//   - empty `requested` → default under `<workspace>/<sid>/data/<short>.<ext>`.
//   - relative path → anchored under `<workspace>/<sid>/data/`.
//   - absolute path → must canonicalise (post-EvalSymlinks) under
//     either `<workspace>/<sid>/` or `<shared>/<aid>/`. Otherwise
//     returns ErrPathEscape.
//
// The per-session workspace dir is created on demand; the shared
// dir is not (the agent is responsible for materialising it).
func (d *queryDeps) resolveOutPath(sessionID, requested, queryID, ext string) (string, error) {
	if d.workspace == "" {
		return "", errors.New("WORKSPACES_ROOT not set")
	}
	if sessionID == "" {
		return "", errors.New("session_id missing in tool call metadata")
	}
	if requested == "" {
		return d.defaultPath(sessionID, queryID, ext)
	}
	cleaned := filepath.Clean(requested)
	if !filepath.IsAbs(cleaned) {
		// Relative paths anchor at the session workspace root, NOT
		// under data/. The LLM picks where to put the file
		// (`data/foo.parquet`, `report.json` at the top, etc.);
		// only the auto-generated default lands under data/.
		cleaned = filepath.Join(d.workspace, sessionID, cleaned)
	}
	if err := ensureParent(cleaned); err != nil {
		return "", err
	}
	abs, err := filepath.Abs(cleaned)
	if err != nil {
		return "", err
	}
	canonical, err := canonicalise(abs)
	if err != nil {
		return "", err
	}
	if !d.underAllowedRoot(canonical, sessionID) {
		return "", &toolError{Code: "path_escape", Msg: requested}
	}
	// Return the as-asked path (abs-clean) so the LLM sees the
	// path it supplied; the canonical form is an implementation
	// detail of the boundary check.
	return abs, nil
}

// defaultPath generates the default output path under the
// session's data sub-directory.
func (d *queryDeps) defaultPath(sessionID, queryID, ext string) (string, error) {
	dir := filepath.Join(d.workspace, sessionID, "data")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(dir, queryID+"."+ext), nil
}

// underAllowedRoot reports whether canonical resolves under either
// the session workspace or the shared dir. Both sides are
// EvalSymlinks-canonicalised so /var/folders → /private/var/folders
// (macOS) doesn't trip the comparison; we mkdir the roots first
// to make the symlink resolution well-defined.
func (d *queryDeps) underAllowedRoot(canonical, sessionID string) bool {
	if root := canonRoot(filepath.Join(d.workspace, sessionID)); hasDirPrefix(canonical, root) {
		return true
	}
	if d.shared != "" && d.agentID != "" {
		if root := canonRoot(filepath.Join(d.shared, d.agentID)); hasDirPrefix(canonical, root) {
			return true
		}
	}
	return false
}

// canonRoot ensures the root exists, then returns its canonical
// absolute path. Failure to mkdir just degrades to the abs-only
// path — the caller's hasDirPrefix check will still correctly
// reject mismatches.
func canonRoot(p string) string {
	_ = os.MkdirAll(p, 0o755)
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

// canonicalise resolves symlinks. For a not-yet-existing leaf it
// canonicalises the parent and rejoins. The caller already
// prepared the parent via ensureParent, so this never fails on a
// fresh write path.
func canonicalise(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return resolved, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	parent := filepath.Dir(abs)
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolvedParent, filepath.Base(abs)), nil
}

// ensureParent makes the parent directory of path. Surface IO
// errors to the caller — they map to tool_error{code:"io"}.
func ensureParent(path string) error {
	return os.MkdirAll(filepath.Dir(path), 0o755)
}

// hasDirPrefix is hasPrefix-on-paths with a trailing-separator
// guard so /workspace/s1abc isn't accidentally matched by
// /workspace/s1.
func hasDirPrefix(p, root string) bool {
	if root == "" {
		return false
	}
	rs := strings.TrimRight(root, string(filepath.Separator))
	if p == rs {
		return true
	}
	return strings.HasPrefix(p, rs+string(filepath.Separator))
}

// newShortID returns 8 random hex characters (32 bits of entropy
// — enough to avoid collisions inside one session's data dir).
// crypto/rand failure is treated as fatal: a deterministic id
// would silently shadow another query's output.
func newShortID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Errorf("crypto/rand: %w", err))
	}
	return hex.EncodeToString(b[:])
}

// writeFileAtomic writes data to path via a temp file and rename.
// Atomicity matters because a partial Parquet file would be
// silently corrupt; same for JSON when the LLM expects a
// well-formed object.
func writeFileAtomic(path string, data []byte) error {
	if err := ensureParent(path); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
