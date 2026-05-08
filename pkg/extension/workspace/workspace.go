package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// tracker owns the per-session scratch directory bookkeeping.
// Each session gets one directory under root. The dir lives until
// release is called (typically on session close). sweepOrphans
// reclaims dirs left behind by sessions that crashed without a
// clean Close — anything older than the configured TTL and absent
// from the live-session set is removed.
//
// tracker is safe for concurrent use; the map is only mutated
// inside acquire / release.
type tracker struct {
	root    string
	cleanup bool

	mu   sync.Mutex
	dirs map[string]string
}

// newTracker constructs a tracker rooted at root. cleanup decides
// whether release deletes the directory or merely forgets it
// (operators debugging crashed sessions sometimes prefer dirs to
// linger).
func newTracker(root string, cleanup bool) *tracker {
	return &tracker{
		root:    root,
		cleanup: cleanup,
		dirs:    make(map[string]string),
	}
}

// Root returns the absolute workspace root, resolved against the
// process working directory at construction.
func (t *tracker) Root() (string, error) {
	if t.root == "" {
		return "", fmt.Errorf("workspace: empty root")
	}
	return filepath.Abs(t.root)
}

// acquire creates the per-session directory under Root and records
// it under the given sessionID. Idempotent: a second call for the
// same sessionID returns the recorded dir without touching the
// filesystem.
func (t *tracker) acquire(sessionID string) (string, error) {
	t.mu.Lock()
	if dir, ok := t.dirs[sessionID]; ok {
		t.mu.Unlock()
		return dir, nil
	}
	t.mu.Unlock()

	root, err := t.Root()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("workspace: mkdir %s: %w", dir, err)
	}
	t.mu.Lock()
	t.dirs[sessionID] = dir
	t.mu.Unlock()
	return dir, nil
}

// release removes the sessionID from the tracker and, when
// cleanup is enabled, deletes the directory. Errors from the
// rmdir are returned but the entry is removed regardless — a
// stuck dir must not block teardown.
func (t *tracker) release(sessionID string) error {
	t.mu.Lock()
	dir, ok := t.dirs[sessionID]
	if ok {
		delete(t.dirs, sessionID)
	}
	t.mu.Unlock()
	if !ok || !t.cleanup {
		return nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("workspace: rm %s: %w", dir, err)
	}
	return nil
}

// sweepOrphans removes child directories of root whose mtime is
// older than ttl and whose name is not in the liveSessions set.
// Returns the count of removed entries. Zero TTL disables the
// sweep.
func (t *tracker) sweepOrphans(liveSessions map[string]struct{}, ttl time.Duration) (int, error) {
	if ttl <= 0 {
		return 0, nil
	}
	root, err := t.Root()
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
