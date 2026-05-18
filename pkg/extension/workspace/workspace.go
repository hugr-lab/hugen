package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// tracker owns the per-session scratch directory bookkeeping.
// Layout decisions (root id at top level, mission ids nested under
// their root, workers sharing the parent mission's dir) live on
// the [Extension]; tracker only mkdirs at whatever relative path
// the caller supplies via acquireAt. Dirs are never deleted on
// session close — sweepOrphans reclaims mission subdirs after a
// configurable TTL, and root-level dirs are deferred to phase-6
// cron (which has the full toolkit for chat-session retention).
//
// tracker is safe for concurrent use; the map is only mutated
// inside acquireAt / forget.
type tracker struct {
	root string

	mu   sync.Mutex
	dirs map[string]string
}

// newTracker constructs a tracker rooted at root.
func newTracker(root string) *tracker {
	return &tracker{
		root: root,
		dirs: make(map[string]string),
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

// acquireAt creates root/<relPath> and records it under sessionID.
// Idempotent: a second call for the same sessionID returns the
// recorded dir without touching the filesystem (so mission resume
// re-binds the same path even if the relative layout changed in
// the meantime).
func (t *tracker) acquireAt(sessionID, relPath string) (string, error) {
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
	dir := filepath.Join(root, relPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("workspace: mkdir %s: %w", dir, err)
	}
	t.mu.Lock()
	t.dirs[sessionID] = dir
	t.mu.Unlock()
	return dir, nil
}

// forget drops the sessionID from the tracker without touching the
// filesystem. Returns the dir that had been recorded (empty when
// the id was unknown) so callers can log it.
func (t *tracker) forget(sessionID string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	dir, ok := t.dirs[sessionID]
	if ok {
		delete(t.dirs, sessionID)
	}
	return dir
}

// sweepOrphans walks <root>/<root_dir>/<mission_dir>/ and removes
// mission subdirs whose mtime is older than ttl and whose name is
// not in liveSessions. Root-level dirs are left alone (chat-session
// cleanup is the phase-6 cron's job). Returns the count of removed
// mission subdirs. A zero TTL disables the sweep.
func (t *tracker) sweepOrphans(liveSessions map[string]struct{}, ttl time.Duration) (int, error) {
	if ttl <= 0 {
		return 0, nil
	}
	root, err := t.Root()
	if err != nil {
		return 0, nil
	}
	rootEntries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	cutoff := time.Now().Add(-ttl)
	removed := 0
	for _, re := range rootEntries {
		if !re.IsDir() {
			continue
		}
		rootDir := filepath.Join(root, re.Name())
		missionEntries, err := os.ReadDir(rootDir)
		if err != nil {
			continue
		}
		for _, me := range missionEntries {
			if !me.IsDir() {
				continue
			}
			if _, live := liveSessions[me.Name()]; live {
				continue
			}
			fi, err := me.Info()
			if err != nil {
				continue
			}
			if fi.ModTime().After(cutoff) {
				continue
			}
			if err := os.RemoveAll(filepath.Join(rootDir, me.Name())); err == nil {
				removed++
			}
		}
	}
	return removed, nil
}
