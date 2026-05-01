// Package spawn defines how named auth sources mint credentials
// for an MCP child process. A `tool_providers` entry references a
// source via `auth: <name>`, and the per-session lifecycle asks
// the matching Source for the env variables to inject and a
// revoke fn to call when the session closes.
//
// Concrete sources live under pkg/auth/sources/<name>/. The
// registry here just keeps the type-name → Source mapping that
// the lifecycle consults.
package spawn

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// Source mints per-spawn credentials for an MCP child process.
//
// The lifecycle calls Env once per session-open per provider that
// declares this source by name. The returned env map is layered on
// top of the operator-provided cfg.Env; revoke is appended to the
// session's teardown set and called on close.
//
// Implementations OWN the credential format (which env keys they
// inject). Callers must not assume a fixed key set.
type Source interface {
	Name() string
	Env(ctx context.Context, sessionID string) (env map[string]string, revoke func(), err error)
}

// Sources is a registry keyed by Source.Name. Safe for concurrent
// use; registrations happen at boot and lookups happen per
// session.Open, but the API is conservative about both.
type Sources struct {
	mu sync.RWMutex
	m  map[string]Source
}

// NewSources returns an empty registry.
func NewSources() *Sources {
	return &Sources{m: make(map[string]Source)}
}

// Register adds src under src.Name(). Re-registration of the same
// name is rejected — sources are wired once at boot and the
// duplicate signals a programming error.
func (s *Sources) Register(src Source) error {
	if src == nil {
		return fmt.Errorf("spawn: register nil source")
	}
	name := src.Name()
	if name == "" {
		return fmt.Errorf("spawn: source has empty name")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.m[name]; dup {
		return fmt.Errorf("spawn: source %q already registered", name)
	}
	s.m[name] = src
	return nil
}

// Get returns the source registered under name, or (nil, false).
func (s *Sources) Get(name string) (Source, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	src, ok := s.m[name]
	return src, ok
}

// Names returns the registered source names sorted alphabetically.
// Used by error messages so the user sees what they could have
// written instead of their typo.
func (s *Sources) Names() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.m))
	for n := range s.m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
