package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Workspace resolves logical bash-mcp paths against the three
// roots: /workspace/<session_id>/, /shared/<agent_id>/, and the
// declared /readonly/<name>/ mounts. Every resolution runs
// filepath.Clean → root-prefix attach → filepath.EvalSymlinks and
// rejects paths whose canonical form escapes the allowed set.
type Workspace struct {
	WorkspaceRoot string         // host path of /workspace
	SharedRoot    string         // host path of /shared
	ReadonlyMnt   []ReadonlyMnt  // declared read-only mounts
	AgentID       string         // for /shared/<agent_id>
	SessionID     string         // for /workspace/<session_id>
	OrphanTTL     time.Duration  // orphan workspace sweep window
}

// ReadonlyMnt is one declared mount under /readonly/<name>/.
type ReadonlyMnt struct {
	Name string // logical name under /readonly/
	Host string // host path; must exist at boot
}

// Errors returned by Workspace.Resolve.
var (
	ErrPathEscape           = errors.New("bash-mcp: path resolves outside allowed roots")
	ErrReadOnly             = errors.New("bash-mcp: write rejected on read-only mount")
	ErrReadonlyMountMissing = errors.New("bash-mcp: declared readonly mount missing")
)

// Resolution is the result of Workspace.Resolve.
type Resolution struct {
	// Canonical is the EvalSymlinks-canonicalised host path. Use
	// this for the actual file-system call.
	Canonical string
	// Logical is the input as logically rooted under one of the
	// three trees ("/workspace/<sid>/foo"). Useful for audit
	// frames so the operator sees what the LLM saw.
	Logical string
	// Root identifies which of the three trees the path is under.
	Root WorkspaceRoot
}

// WorkspaceRoot enumerates the three trees.
type WorkspaceRoot int

const (
	RootWorkspace WorkspaceRoot = iota
	RootShared
	RootReadOnly
)

// Validate checks that the workspace can boot: every declared
// readonly mount must exist on the host, and the workspace and
// shared roots must be writable directories.
func (w *Workspace) Validate() error {
	if w.WorkspaceRoot == "" {
		return errors.New("bash-mcp: empty workspace root")
	}
	if w.SharedRoot == "" {
		return errors.New("bash-mcp: empty shared root")
	}
	if w.SessionID == "" {
		return errors.New("bash-mcp: empty session id")
	}
	if w.AgentID == "" {
		return errors.New("bash-mcp: empty agent id")
	}
	for _, m := range w.ReadonlyMnt {
		if m.Name == "" || m.Host == "" {
			return fmt.Errorf("bash-mcp: invalid readonly mount %+v", m)
		}
		fi, err := os.Stat(m.Host)
		if err != nil || !fi.IsDir() {
			return fmt.Errorf("%w: %s -> %s", ErrReadonlyMountMissing, m.Name, m.Host)
		}
	}
	return nil
}

// EnsureSessionDirs creates the session's /workspace/<sid>/ and
// /shared/<aid>/ host paths. Idempotent.
func (w *Workspace) EnsureSessionDirs() error {
	sess := filepath.Join(w.WorkspaceRoot, w.SessionID)
	shared := filepath.Join(w.SharedRoot, w.AgentID)
	for _, p := range []string{sess, shared} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			return fmt.Errorf("bash-mcp: ensure %s: %w", p, err)
		}
	}
	return nil
}

// Resolve canonicalises an input path. write=true rejects paths
// under /readonly/.
func (w *Workspace) Resolve(input string, write bool) (Resolution, error) {
	if input == "" {
		return Resolution{}, fmt.Errorf("%w: empty path", ErrPathEscape)
	}
	clean := filepath.Clean(input)
	logical, root, err := w.attachRoot(clean)
	if err != nil {
		return Resolution{}, err
	}
	hostPath := w.hostPath(logical, root)
	canonical, err := canonicalise(hostPath)
	if err != nil {
		return Resolution{}, err
	}
	// Re-check the canonical path is still under the right root —
	// covers symlinks pointing outside the tree.
	if !w.canonicalUnderRoot(canonical, root) {
		return Resolution{}, fmt.Errorf("%w: %s -> %s", ErrPathEscape, input, canonical)
	}
	if write && root == RootReadOnly {
		return Resolution{}, fmt.Errorf("%w: %s", ErrReadOnly, input)
	}
	return Resolution{Canonical: canonical, Logical: logical, Root: root}, nil
}

// attachRoot maps a cleaned input path to one of the three trees.
// Relative paths default to /workspace/<sid>/.
func (w *Workspace) attachRoot(clean string) (string, WorkspaceRoot, error) {
	if !filepath.IsAbs(clean) {
		return "/workspace/" + w.SessionID + "/" + clean, RootWorkspace, nil
	}
	switch {
	case strings.HasPrefix(clean, "/workspace/"+w.SessionID):
		return clean, RootWorkspace, nil
	case strings.HasPrefix(clean, "/workspace/"):
		// Cross-session traversal forbidden.
		return "", 0, fmt.Errorf("%w: %s (cross-session)", ErrPathEscape, clean)
	case strings.HasPrefix(clean, "/shared/"+w.AgentID):
		return clean, RootShared, nil
	case strings.HasPrefix(clean, "/shared/"):
		return "", 0, fmt.Errorf("%w: %s (cross-agent)", ErrPathEscape, clean)
	case strings.HasPrefix(clean, "/readonly/"):
		// Match against any declared mount.
		rest := strings.TrimPrefix(clean, "/readonly/")
		parts := strings.SplitN(rest, "/", 2)
		for _, m := range w.ReadonlyMnt {
			if m.Name == parts[0] {
				return clean, RootReadOnly, nil
			}
		}
		return "", 0, fmt.Errorf("%w: %s (unknown readonly mount)", ErrPathEscape, clean)
	default:
		return "", 0, fmt.Errorf("%w: %s", ErrPathEscape, clean)
	}
}

// hostPath maps a logical path under one of the three trees to
// the host-side path.
func (w *Workspace) hostPath(logical string, root WorkspaceRoot) string {
	switch root {
	case RootWorkspace:
		rest := strings.TrimPrefix(logical, "/workspace/"+w.SessionID)
		return filepath.Join(w.WorkspaceRoot, w.SessionID, rest)
	case RootShared:
		rest := strings.TrimPrefix(logical, "/shared/"+w.AgentID)
		return filepath.Join(w.SharedRoot, w.AgentID, rest)
	case RootReadOnly:
		// /readonly/<name>/<rest> -> ReadonlyMnt[name].Host/<rest>
		rest := strings.TrimPrefix(logical, "/readonly/")
		parts := strings.SplitN(rest, "/", 2)
		for _, m := range w.ReadonlyMnt {
			if m.Name == parts[0] {
				if len(parts) == 1 {
					return m.Host
				}
				return filepath.Join(m.Host, parts[1])
			}
		}
	}
	return ""
}

// canonicalUnderRoot returns true if canonical is under the host
// directory backing root.
func (w *Workspace) canonicalUnderRoot(canonical string, root WorkspaceRoot) bool {
	switch root {
	case RootWorkspace:
		return underHostDir(canonical, filepath.Join(w.WorkspaceRoot, w.SessionID))
	case RootShared:
		return underHostDir(canonical, filepath.Join(w.SharedRoot, w.AgentID))
	case RootReadOnly:
		for _, m := range w.ReadonlyMnt {
			if underHostDir(canonical, m.Host) {
				return true
			}
		}
	}
	return false
}

func underHostDir(child, parent string) bool {
	pAbs, err := filepath.Abs(parent)
	if err != nil {
		return false
	}
	pCanon, err := filepath.EvalSymlinks(pAbs)
	if err != nil {
		pCanon = pAbs
	}
	rel, err := filepath.Rel(pCanon, child)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, "..") && rel != "..")
}

// canonicalise runs filepath.EvalSymlinks. If the path doesn't
// exist yet (writes that create new files), canonicalise the
// parent directory and re-attach the basename.
func canonicalise(host string) (string, error) {
	if _, err := os.Lstat(host); err == nil {
		return filepath.EvalSymlinks(host)
	}
	parent := filepath.Dir(host)
	base := filepath.Base(host)
	if _, err := os.Stat(parent); err != nil {
		// Parent does not exist either; bash.write_file may need
		// to create parents — return the input as-is, callers can
		// MkdirAll the parent and re-resolve.
		return host, nil
	}
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, base), nil
}

// SweepOrphans removes session workspaces older than OrphanTTL.
// Returns the count of removed entries.
func (w *Workspace) SweepOrphans() (int, error) {
	if w.OrphanTTL <= 0 {
		return 0, nil
	}
	entries, err := os.ReadDir(w.WorkspaceRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	cutoff := time.Now().Add(-w.OrphanTTL)
	removed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if e.Name() == w.SessionID {
			// Live session.
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if fi.ModTime().Before(cutoff) {
			path := filepath.Join(w.WorkspaceRoot, e.Name())
			if err := os.RemoveAll(path); err == nil {
				removed++
			}
		}
	}
	return removed, nil
}
