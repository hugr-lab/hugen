package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Workspace is the thin host-fs view bash-mcp's convenience tools
// (read_file, write_file, list_dir, sed) use to canonicalise input
// paths and enforce one safety property: never let a tool reach
// into a peer session's scratch directory.
//
// Two roots matter:
//
//   - SessionDir — this process's own scratch (= cwd at startup,
//     the runtime sets cmd.Dir to <workspaces>/<session_id>/).
//   - WorkspacesRoot — the parent of every session scratch. Any
//     canonical path under this root that is not under
//     SessionDir belongs to another session — the file tools
//     reject access to it.
//
// Outside these two roots the host filesystem is open. Shell
// tools (bash.run/shell) inherit the same SESSION_DIR /
// WORKSPACES_ROOT environment variables but bash-mcp does not
// gate their args at exec time — kernel/OS isolation in the
// deployment is responsible there.
type Workspace struct {
	SessionDir     string // absolute path; must equal os.Getwd() at start
	WorkspacesRoot string // absolute parent of SessionDir
}

// Errors returned by Workspace.Resolve.
var (
	ErrPathEscape       = errors.New("bash-mcp: path resolves outside allowed roots")
	ErrCrossSessionPath = errors.New("bash-mcp: path resolves into another session's workspace")
)

// Resolution is the result of Workspace.Resolve.
type Resolution struct {
	Canonical string // EvalSymlinks-resolved host path
	Logical   string // path as the caller supplied it (cleaned)
}

// Resolve canonicalises an input path and enforces the
// cross-session boundary: any canonical path under WorkspacesRoot
// must also be under SessionDir, otherwise it belongs to a peer
// session. Outside WorkspacesRoot the path is unconstrained.
//
// The `write` parameter is preserved for HITL approval routing
// in phase 5 — phase 3 applies the same boundary check to both
// reads and writes.
func (w *Workspace) Resolve(input string, write bool) (Resolution, error) {
	_ = write
	if input == "" {
		return Resolution{}, fmt.Errorf("%w: empty path", ErrPathEscape)
	}
	clean := filepath.Clean(input)
	canonical, err := canonicalise(clean)
	if err != nil {
		return Resolution{}, err
	}
	if w.WorkspacesRoot != "" && w.SessionDir != "" {
		if underHostDir(canonical, w.WorkspacesRoot) && !underHostDir(canonical, w.SessionDir) {
			return Resolution{}, fmt.Errorf("%w: %s", ErrCrossSessionPath, input)
		}
	}
	return Resolution{Canonical: canonical, Logical: clean}, nil
}

// underHostDir reports whether `child` is the same path as or a
// descendant of `parent`. Both are canonicalised (filepath.Abs +
// EvalSymlinks where the dir exists) before comparison so symlink
// trickery cannot bypass the check.
func underHostDir(child, parent string) bool {
	pAbs, err := filepath.Abs(parent)
	if err != nil {
		return false
	}
	if eval, err := filepath.EvalSymlinks(pAbs); err == nil {
		pAbs = eval
	}
	cAbs := child
	if !filepath.IsAbs(cAbs) {
		if abs, err := filepath.Abs(cAbs); err == nil {
			cAbs = abs
		}
	}
	rel, err := filepath.Rel(pAbs, cAbs)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if strings.HasPrefix(rel, "..") {
		return false
	}
	return true
}

// canonicalise runs filepath.EvalSymlinks. If the path doesn't
// exist yet (writes that create new files), canonicalise the
// parent directory and re-attach the basename.
func canonicalise(host string) (string, error) {
	if !filepath.IsAbs(host) {
		if abs, err := filepath.Abs(host); err == nil {
			host = abs
		}
	}
	if _, err := os.Lstat(host); err == nil {
		return filepath.EvalSymlinks(host)
	}
	parent := filepath.Dir(host)
	base := filepath.Base(host)
	if _, err := os.Stat(parent); err != nil {
		return host, nil
	}
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, base), nil
}
