package session

import (
	"context"
	"testing"
)

func TestSessionContext_RoundTrip(t *testing.T) {
	s := &Session{id: "ses-fixture"}
	ctx := WithSession(context.Background(), s)
	got, ok := SessionFromContext(ctx)
	if !ok {
		t.Fatal("SessionFromContext: ok=false, want true")
	}
	if got != s {
		t.Errorf("got %p, want %p", got, s)
	}
}

func TestSessionContext_Empty(t *testing.T) {
	if got, ok := SessionFromContext(context.Background()); ok || got != nil {
		t.Errorf("empty ctx: got=%v ok=%v, want nil/false", got, ok)
	}
}

func TestSessionContext_NilSession(t *testing.T) {
	// WithSession with a nil *Session is a no-op (would otherwise
	// stash an interface holding a typed nil, which type-asserts to
	// (*Session)(nil), true — surprising to callers).
	ctx := WithSession(context.Background(), nil)
	if got, ok := SessionFromContext(ctx); ok || got != nil {
		t.Errorf("WithSession(ctx, nil) leaked nil session: got=%v ok=%v", got, ok)
	}
}
