package session

import "context"

// sessionCtxKey is the unexported key under which dispatchToolCall
// stashes the live *Session for the call's ctx. Tools that need
// session-scoped state (whiteboard, plan, sub-agent registry, parent
// linkage) read it via SessionFromContext rather than reaching into
// Manager — which is now root-only and would not surface a sub-agent
// caller.
//
// The context-key idiom — unexported struct{} value — keeps the slot
// private to this package: external code can only read/write via
// WithSession / SessionFromContext, so no foreign package can plant a
// rogue *Session pointer here.
type sessionCtxKey struct{}

// WithSession returns a child ctx carrying s. Used by
// session.dispatchToolCall to wire the calling session into the tool
// dispatch ctx so per-session ToolProviders (Manager-as-ToolProvider
// for the session-scoped tools, the analyst skill_files provider,
// etc.) can recover their *Session without a stringly-typed lookup
// through Manager.Get.
func WithSession(ctx context.Context, s *Session) context.Context {
	if ctx == nil || s == nil {
		return ctx
	}
	return context.WithValue(ctx, sessionCtxKey{}, s)
}

// SessionFromContext returns the *Session stashed by WithSession,
// or nil if the ctx has no session attached. The bool return form
// lets callers distinguish "no session" from "nil session" (the
// latter is impossible by construction but the bool keeps the API
// uniform with the standard ctx-value patterns).
func SessionFromContext(ctx context.Context) (*Session, bool) {
	if ctx == nil {
		return nil, false
	}
	s, ok := ctx.Value(sessionCtxKey{}).(*Session)
	if !ok || s == nil {
		return nil, false
	}
	return s, true
}
