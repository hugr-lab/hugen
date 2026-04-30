package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Workspace resolves bash-mcp logical paths against three roots:
//
//   - the process cwd        — the session-scoped writable root.
//     bash-mcp is started by the runtime with cmd.Dir set to
//     <workspace_dir>/<session_id>/ (host-side) or /workspace/
//     (in-container). bash-mcp itself never names the session id.
//   - /shared/                — agent-wide, writable when configured.
//   - /readonly/<mount>/      — operator-declared read-only mounts.
//
// Every resolution runs filepath.Clean → root-attach →
// filepath.EvalSymlinks and rejects paths whose canonical form
// escapes the allowed set.
type Workspace struct {
	SharedRoot     string        // host path of /shared (empty → disabled)
	SharedWritable bool          // bash.write_file allowed under /shared
	ReadonlyMnt    []ReadonlyMnt // declared read-only mounts
}

// ReadonlyMnt is one declared mount under /readonly/<name>/.
type ReadonlyMnt struct {
	Name string `json:"name"`
	Host string `json:"path"`
}

// Errors returned by Workspace.Resolve.
var (
	ErrPathEscape           = errors.New("bash-mcp: path resolves outside allowed roots")
	ErrReadOnly             = errors.New("bash-mcp: write rejected on read-only mount")
	ErrReadonlyMountMissing = errors.New("bash-mcp: declared readonly mount missing")
	ErrSharedDisabled       = errors.New("bash-mcp: /shared not configured")
)

// Resolution is the result of Workspace.Resolve.
type Resolution struct {
	Canonical string        // EvalSymlinks-resolved host path
	Logical   string        // path as it appeared (or the cwd-relative form)
	Root      WorkspaceRoot // tree the path resolved under
}

// WorkspaceRoot enumerates the three trees.
type WorkspaceRoot int

const (
	RootSession WorkspaceRoot = iota // cwd
	RootShared
	RootReadOnly
)

// Validate checks every declared readonly mount exists at boot.
func (w *Workspace) Validate() error {
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

// Resolve canonicalises an input path. write=true rejects paths
// under /readonly/ and (when SharedWritable is false) /shared/.
func (w *Workspace) Resolve(input string, write bool) (Resolution, error) {
	if input == "" {
		return Resolution{}, fmt.Errorf("%w: empty path", ErrPathEscape)
	}
	clean := filepath.Clean(input)
	hostPath, root, err := w.attachRoot(clean)
	if err != nil {
		return Resolution{}, err
	}
	canonical, err := canonicalise(hostPath)
	if err != nil {
		return Resolution{}, err
	}
	if !w.canonicalUnderRoot(canonical, root) {
		return Resolution{}, fmt.Errorf("%w: %s -> %s", ErrPathEscape, input, canonical)
	}
	if write {
		switch root {
		case RootReadOnly:
			return Resolution{}, fmt.Errorf("%w: %s", ErrReadOnly, input)
		case RootShared:
			if !w.SharedWritable {
				return Resolution{}, fmt.Errorf("%w: %s (shared read-only)", ErrReadOnly, input)
			}
		}
	}
	return Resolution{Canonical: canonical, Logical: clean, Root: root}, nil
}

// attachRoot maps a cleaned input path to its host equivalent.
// Relative paths default to the cwd (session root).
func (w *Workspace) attachRoot(clean string) (string, WorkspaceRoot, error) {
	if !filepath.IsAbs(clean) {
		// Relative → cwd-anchored. Pass through unchanged so
		// canonicalise resolves against os.Getwd().
		return clean, RootSession, nil
	}
	switch {
	case strings.HasPrefix(clean, "/shared/") || clean == "/shared":
		if w.SharedRoot == "" {
			return "", 0, fmt.Errorf("%w: %s", ErrSharedDisabled, clean)
		}
		rest := strings.TrimPrefix(clean, "/shared")
		return filepath.Join(w.SharedRoot, rest), RootShared, nil
	case strings.HasPrefix(clean, "/readonly/"):
		rest := strings.TrimPrefix(clean, "/readonly/")
		parts := strings.SplitN(rest, "/", 2)
		for _, m := range w.ReadonlyMnt {
			if m.Name == parts[0] {
				if len(parts) == 1 {
					return m.Host, RootReadOnly, nil
				}
				return filepath.Join(m.Host, parts[1]), RootReadOnly, nil
			}
		}
		return "", 0, fmt.Errorf("%w: %s (unknown readonly mount)", ErrPathEscape, clean)
	default:
		return "", 0, fmt.Errorf("%w: %s", ErrPathEscape, clean)
	}
}

// canonicalUnderRoot returns true if canonical is under the host
// directory backing root.
func (w *Workspace) canonicalUnderRoot(canonical string, root WorkspaceRoot) bool {
	switch root {
	case RootSession:
		cwd, err := os.Getwd()
		if err != nil {
			return false
		}
		return underHostDir(canonical, cwd)
	case RootShared:
		return underHostDir(canonical, w.SharedRoot)
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
	cAbs := child
	if !filepath.IsAbs(cAbs) {
		if abs, err := filepath.Abs(cAbs); err == nil {
			cAbs = abs
		}
	}
	rel, err := filepath.Rel(pCanon, cAbs)
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
		return host, nil
	}
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, base), nil
}
