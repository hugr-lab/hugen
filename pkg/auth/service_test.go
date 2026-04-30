package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hugr-lab/hugen/pkg/auth/sources"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubSource is a minimal Source used to test SourceRegistry and
// the /auth/callback dispatcher in isolation.
type stubSource struct {
	name      string
	token     string
	callbacks int
}

func (s *stubSource) Name() string                          { return s.name }
func (s *stubSource) Token(context.Context) (string, error) { return s.token, nil }
func (s *stubSource) Login(context.Context) error           { return nil }
func (s *stubSource) OwnsState(state string) bool           { return sources.StateOwnedBy(s.name, state) }
func (s *stubSource) HandleCallback(w http.ResponseWriter, _ *http.Request) {
	s.callbacks++
	w.WriteHeader(http.StatusOK)
}

func TestStateOwnedBy(t *testing.T) {
	assert.True(t, sources.StateOwnedBy("hugr", "hugr.abc123"))
	assert.False(t, sources.StateOwnedBy("hugr", "hugr"))    // no dot suffix
	assert.False(t, sources.StateOwnedBy("hugr", "foo.abc")) // wrong source
	assert.False(t, sources.StateOwnedBy("hugr", ""))
	assert.Equal(t, "hugr.nonce-1", sources.EncodeState("hugr", "nonce-1"))
}

func TestService_AddAndAlias(t *testing.T) {
	mux := http.NewServeMux()
	reg := NewService(nil, mux, "")
	a := &stubSource{name: "hugr", token: "a-token"}

	require.NoError(t, reg.Add(a))
	require.ErrorContains(t, reg.Add(a), "duplicate source name")

	// Alias a second name to it.
	require.NoError(t, reg.Alias("hugr-mcp", "hugr"))
	require.ErrorContains(t, reg.Alias("hugr-mcp", "hugr"), "duplicate alias")
	require.ErrorContains(t, reg.Alias("hugr", "hugr"), "points to itself")
	require.ErrorContains(t, reg.Alias("missing", "ghost"), "does not exist")

	got, ok := reg.Source("hugr-mcp")
	require.True(t, ok)
	assert.Same(t, sources.Source(a), got, "alias should resolve to target")

	// TokenStores snapshot covers both direct names and aliases.
	snap := reg.TokenStores()
	assert.Contains(t, snap, "hugr")
	assert.Contains(t, snap, "hugr-mcp")
}

func TestService_DispatchByState(t *testing.T) {
	mux := http.NewServeMux()
	reg := NewService(nil, mux, "")
	a := &stubSource{name: "hugr"}
	b := &stubSource{name: "weather"}
	require.NoError(t, reg.Add(a))
	require.NoError(t, reg.Add(b))

	// State belongs to weather.
	req := httptest.NewRequest(http.MethodGet,
		"/auth/callback?state="+sources.EncodeState("weather", "nonce")+"&code=xyz", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 1, b.callbacks)
	assert.Equal(t, 0, a.callbacks)

	// State belongs to hugr.
	req = httptest.NewRequest(http.MethodGet,
		"/auth/callback?state="+sources.EncodeState("hugr", "other")+"&code=xyz", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 1, a.callbacks)

	// Unknown state → 400, no handler invoked.
	req = httptest.NewRequest(http.MethodGet, "/auth/callback?state=ghost.x", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, 1, a.callbacks)
	assert.Equal(t, 1, b.callbacks)

	// Missing state → 400.
	req = httptest.NewRequest(http.MethodGet, "/auth/callback", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestService_PromptLogins(t *testing.T) {
	mux := http.NewServeMux()
	reg := NewService(nil, mux, "")
	called := 0
	reg.RegisterPromptLogin(func() { called++ })
	reg.RegisterPromptLogin(func() { called++ })
	hooks := reg.PromptLogins()
	require.Len(t, hooks, 2)
	for _, h := range hooks {
		h()
	}
	assert.Equal(t, 2, called)
}
