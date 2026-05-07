package session

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Workspace owns the per-session scratch directory bookkeeping.
//
// Each session gets one directory under the configured root. The
// dir lives until Release is called (typically on session close).
// SweepOrphans reclaims dirs left behind by sessions that crashed
// without a clean Close — anything older than the configured TTL
// and absent from the live-session set is removed.
//
// Workspace is safe for concurrent use; the manager mutates the
// map only inside Acquire / Release.
type Workspace struct {
	root    string
	cleanup bool

	mu   sync.Mutex
	dirs map[string]string
}

// NewWorkspace constructs a workspace tracker rooted at the given
// directory. cleanup decides whether Release deletes the
// directory or merely forgets it (operators debugging crashed
// sessions sometimes prefer dirs to linger).
func NewWorkspace(root string, cleanup bool) *Workspace {
	return &Workspace{
		root:    root,
		cleanup: cleanup,
		dirs:    make(map[string]string),
	}
}

// Root returns the absolute workspace root, resolved against the
// process working directory at construction.
func (w *Workspace) Root() (string, error) {
	if w.root == "" {
		return "", fmt.Errorf("workspace: empty root")
	}
	return filepath.Abs(w.root)
}

// Get returns the session directory recorded for sessionID, or
// ("", false) when the session has not been Acquired (or has
// since been Released). Read-after-acquire helper used by
// extensions that need session-scoped paths without touching the
// Workspace lifecycle.
func (w *Workspace) Get(sessionID string) (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	dir, ok := w.dirs[sessionID]
	return dir, ok
}

// Acquire creates the per-session directory under Root and records
// it under the given sessionID. Idempotent: a second call for the
// same sessionID returns the recorded dir without touching the
// filesystem.
func (w *Workspace) Acquire(sessionID string) (string, error) {
	w.mu.Lock()
	if dir, ok := w.dirs[sessionID]; ok {
		w.mu.Unlock()
		return dir, nil
	}
	w.mu.Unlock()

	root, err := w.Root()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("workspace: mkdir %s: %w", dir, err)
	}
	w.mu.Lock()
	w.dirs[sessionID] = dir
	w.mu.Unlock()
	return dir, nil
}

// Release removes the sessionID from the tracker and, when cleanup
// is enabled, deletes the directory. The returned dir is the path
// that was tracked (empty when sessionID was unknown). Errors from
// the rmdir are returned but the entry is removed regardless — a
// stuck dir must not block the manager from finishing Close.
func (w *Workspace) Release(sessionID string) (string, error) {
	w.mu.Lock()
	dir, ok := w.dirs[sessionID]
	if ok {
		delete(w.dirs, sessionID)
	}
	w.mu.Unlock()
	if !ok || !w.cleanup {
		return dir, nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return dir, fmt.Errorf("workspace: rm %s: %w", dir, err)
	}
	return dir, nil
}

// SweepOrphans removes child directories of Root whose mtime is
// older than ttl and whose name is not in the liveSessions set.
// Returns the count of removed entries. Zero TTL disables the
// sweep.
func (w *Workspace) SweepOrphans(liveSessions map[string]struct{}, ttl time.Duration) (int, error) {
	if ttl <= 0 {
		return 0, nil
	}
	root, err := w.Root()
	if err != nil {
		return 0, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	cutoff := time.Now().Add(-ttl)
	removed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, live := liveSessions[e.Name()]; live {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if fi.ModTime().After(cutoff) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, e.Name())); err == nil {
			removed++
		}
	}
	return removed, nil
}
