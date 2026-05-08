package session

import "context"

// NewRootSession is the cross-package entry point for the root-session
// constructor. Used by pkg/session/manager.Manager.Open. Internal
// callers (Session.Spawn, recover paths) continue to use newSession
// directly because they live inside the pkg/session package and may
// pass parent != nil.
//
// The pkg/session package already exports a NewSession test-fixture
// constructor with a different signature (id + agent + store + ...),
// so this wrapper uses a distinct name to avoid the collision.
func NewRootSession(ctx context.Context, deps *Deps, req OpenRequest) (*Session, error) {
	return newSession(ctx, nil, deps, req)
}

// ResumeSession is the cross-package entry point for the
// resume-from-store constructor. Used by pkg/session/manager.Manager
// .Resume. Internal callers (recover walker, Spawn restoring a
// dangling child) continue to use newSessionRestore directly.
func ResumeSession(ctx context.Context, id string, deps *Deps) (*Session, error) {
	return newSessionRestore(ctx, id, nil, deps)
}
