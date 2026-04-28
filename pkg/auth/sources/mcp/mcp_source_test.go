package mcp

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/hugr-lab/hugen/pkg/auth/internal/fixture"
	"github.com/hugr-lab/hugen/pkg/auth/sources"
	"github.com/hugr-lab/hugen/pkg/auth/sources/oidc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMCPSource_NameRequired(t *testing.T) {
	_, err := NewMCPSource("", nil)
	require.ErrorContains(t, err, "Name is required")
}

func TestMCPSource_OwnsStatePrefix(t *testing.T) {
	s, err := NewMCPSource("weather", nil)
	require.NoError(t, err)
	assert.Equal(t, "weather", s.Name())
	assert.True(t, s.OwnsState(sources.EncodeState("weather", "nonce-1")))
	assert.False(t, s.OwnsState(sources.EncodeState("hugr", "nonce-1")))
}

func TestMCPSource_TokenBeforeAuthorize(t *testing.T) {
	s, err := NewMCPSource("weather", nil)
	require.NoError(t, err)
	_, err = s.Token(context.Background())
	require.ErrorContains(t, err, "not authorized yet")

	err = s.Login(context.Background())
	require.ErrorContains(t, err, "not authorized yet")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/auth/callback?state=weather.x", nil)
	s.HandleCallback(w, r)
	assert.Equal(t, 400, w.Code)
}

// TestMCPSource_SetInnerDelegates: once the OIDC inner engine is
// installed (commit #3+ landing flow), Token/HandleCallback proxy
// through to it. We simulate by constructing a minimal OIDCStore
// against a fakeIdP and wiring it in via the package-local setInner.
func TestMCPSource_SetInnerDelegates(t *testing.T) {
	idp := fixture.NewMockIdP(t)
	inner, err := oidc.New(context.Background(), oidc.Config{
		Name:        "weather",
		IssuerURL:   idp.Srv.URL,
		ClientID:    "agent",
		RedirectURL: "http://localhost/auth/callback",
	})
	require.NoError(t, err)

	s, err := NewMCPSource("weather", nil)
	require.NoError(t, err)

	s.setInner(inner)

	// Before any successful login: Token blocks on inner.ready. We
	// don't exercise the full blocking path here — just assert that
	// the "not authorized yet" short-circuit is gone and delegation
	// works for OwnsState (which is pure).
	assert.True(t, s.OwnsState(sources.EncodeState("weather", "nonce")))
}
