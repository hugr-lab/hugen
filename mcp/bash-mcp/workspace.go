package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Workspace is the thin host-fs view bash-mcp's convenience file
// tools (read_file, write_file, edit_file, list_dir, sed) use to
// canonicalise input paths and enforce two direction-split safety
// properties (see Resolve):
//
//   - WRITES (write_file / edit_file / sed) are confined to the
//     session's own workspace — a host path or a peer session's
//     dir is rejected. A workspace-confined write is inherently
//     safe, so it carries no approval prompt; host deliverables go
//     through artifact:publish, deliberate host writes through the
//     gated bash.shell.
//   - READS keep the host filesystem open (operator-provided
//     inputs, $SHARED_DIR, skill bundles) with one guard: never
//     reach into a peer session's scratch under the shared root.
//
// Two roots matter:
//
//   - SessionDir — this process's own scratch (= cwd at startup,
//     the runtime sets cmd.Dir to <workspaces>/<session_id>/).
//   - WorkspacesRoot — the parent of every session scratch; a
//     canonical path under it but not under SessionDir belongs to
//     another session.
//
// Shell tools (bash.run/shell) inherit the same SESSION_DIR /
// WORKSPACES_ROOT environment variables but bash-mcp does not gate
// their args at exec time — kernel/OS isolation in the deployment
// is responsible there.
type Workspace struct {
	SessionDir     string // absolute path; must equal os.Getwd() at start
	WorkspacesRoot string // absolute parent of SessionDir
}

// Errors returned by Workspace.Resolve.
var (
	ErrPathEscape       = errors.New("bash-mcp: path resolves outside allowed roots")
	ErrCrossSessionPath = errors.New("bash-mcp: path resolves into another session's workspace")
	ErrHostWriteDenied  = errors.New("bash-mcp: file-write tools are confined to your session workspace — deliver host files with artifact:publish, or write outside the workspace via bash.shell")
)

// Resolution is the result of Workspace.Resolve.
type Resolution struct {
	Canonical string // EvalSymlinks-resolved host path
	Logical   string // path as the caller supplied it (cleaned)
}

// Resolve canonicalises an input path and enforces the
// direction-split boundary:
//
//   - write == true (write_file / edit_file / sed): the canonical
//     path MUST be under SessionDir; a host path or a peer
//     session's dir is rejected with ErrHostWriteDenied. The file
//     tools never write outside the workspace, so a workspace
//     write is inherently safe and needs no approval.
//   - write == false (reads): the host filesystem stays open, with
//     the single cross-session guard — a path under WorkspacesRoot
//     that is not under SessionDir belongs to a peer session and is
//     rejected with ErrCrossSessionPath.
//
// Both confines are gated on SessionDir being set; an unconfigured
// Workspace (host mode, test fixtures) leaves everything open.
func (w *Workspace) Resolve(input string, write bool) (Resolution, error) {
	if input == "" {
		return Resolution{}, fmt.Errorf("%w: empty path", ErrPathEscape)
	}
	// Expand environment variables ($SESSION_DIR, $HOME, …) and a
	// leading ~ in the path argument. The shell does this for bash.run
	// / bash.shell commands, but the file tools (read/write/list/sed)
	// take the path directly — without this, a weak model that writes
	// `$SESSION_DIR/out.html` or `~/Downloads/x` as the `path` arg gets
	// a literal, unresolvable path. The cross-session confine below
	// still runs on the EXPANDED canonical path, so this can't widen
	// reach. SESSION_DIR / WORKSPACES_ROOT are set on this process's
	// env by the runtime (pkg/extension/mcp).
	input = expandPath(input)
	if input == "" {
		return Resolution{}, fmt.Errorf("%w: path expanded to empty", ErrPathEscape)
	}
	clean := filepath.Clean(input)
	canonical, err := canonicalise(clean)
	if err != nil {
		return Resolution{}, err
	}
	if w.SessionDir != "" {
		inSession := underHostDir(canonical, w.SessionDir)
		switch {
		case write && !inSession:
			return Resolution{}, fmt.Errorf("%w: %s", ErrHostWriteDenied, input)
		case !write && w.WorkspacesRoot != "" &&
			underHostDir(canonical, w.WorkspacesRoot) && !inSession:
			return Resolution{}, fmt.Errorf("%w: %s", ErrCrossSessionPath, input)
		}
	}
	return Resolution{Canonical: canonical, Logical: clean}, nil
}

// expandPath resolves shell-style placeholders in a file-tool path
// argument: environment variables via os.ExpandEnv ($SESSION_DIR,
// $HOME, ${VAR}) and a leading ~ / ~/ to the user's home directory.
// Unset variables expand to "" (os.ExpandEnv semantics) — the caller
// treats a fully-empty result as an error. Only a LEADING ~ is
// special (mid-path ~ is a legal filename character).
func expandPath(input string) string {
	s := os.ExpandEnv(input)
	switch {
	case s == "~":
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	case strings.HasPrefix(s, "~/"):
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, s[2:])
		}
	}
	return s
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
