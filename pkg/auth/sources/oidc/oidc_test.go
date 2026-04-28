package oidc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/auth/internal/fixture"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOIDCStore_StatePrefixAndLoginCallback(t *testing.T) {
	idp := fixture.NewMockIdP(t)

	// Our RedirectURL points back at ourselves; we'll hand-fire the
	// callback through SourceRegistry rather than over HTTP.
	redirect := "http://localhost:10000/auth/callback"
	store, err := New(context.Background(), Config{
		Name:        "hugr",
		IssuerURL:   idp.Srv.URL,
		ClientID:    "agent",
		RedirectURL: redirect,
	})
	require.NoError(t, err)

	// Step 1: drive HandleLogin directly — should redirect to /authorize
	// with state prefixed by the source name. The shared mux + Service
	// dispatcher are tested at the auth package level; here we exercise
	// the Source in isolation to avoid an import cycle.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/auth/login/hugr", nil)
	store.HandleLogin(w, r)
	require.Equal(t, http.StatusFound, w.Code)

	authorizeURL, err := url.Parse(w.Header().Get("Location"))
	require.NoError(t, err)
	q := authorizeURL.Query()
	assert.Equal(t, "agent", q.Get("client_id"))
	assert.Equal(t, "S256", q.Get("code_challenge_method"))
	assert.True(t, strings.HasPrefix(q.Get("state"), "hugr."),
		"state must be namespaced by source name, got %q", q.Get("state"))

	// Step 2: hit the IdP's /authorize directly with a non-following
	// client so we capture the Location header without the client
	// actually trying to reach the (non-existent) redirect_uri host.
	noFollow := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	idpResp, err := noFollow.Get(authorizeURL.String())
	require.NoError(t, err)
	_ = idpResp.Body.Close()
	require.Equal(t, http.StatusFound, idpResp.StatusCode)

	callbackLoc, err := idpResp.Location()
	require.NoError(t, err)
	code := callbackLoc.Query().Get("code")
	state := callbackLoc.Query().Get("state")
	require.NotEmpty(t, code)
	require.NotEmpty(t, state)

	// Hand-fire the callback directly into the Source. The Service-
	// level dispatcher routing by state prefix is covered separately.
	cw := httptest.NewRecorder()
	cr := httptest.NewRequest(http.MethodGet, "/auth/callback?code="+code+"&state="+state, nil)
	store.HandleCallback(cw, cr)
	require.Equalf(t, http.StatusOK, cw.Code, "callback body: %s", cw.Body.String())
	assert.Contains(t, cw.Body.String(), "Login successful")

	// Step 3: Token() now returns the access token without blocking.
	tok, err := store.Token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "access-token-1", tok)

	// Token endpoint received an authorization_code exchange.
	assert.Equal(t, "authorization_code", idp.LastTokenForm.Get("grant_type"))
	assert.Equal(t, "fake-code", idp.LastTokenForm.Get("code"))
}

func TestOIDCStore_UnknownStateRejected(t *testing.T) {
	idp := fixture.NewMockIdP(t)
	store, err := New(context.Background(), Config{
		Name:        "hugr",
		IssuerURL:   idp.Srv.URL,
		ClientID:    "agent",
		RedirectURL: "http://localhost/auth/callback",
	})
	require.NoError(t, err)

	// Callback with a state that this store never issued.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/auth/callback?code=x&state=hugr.ghost", nil)
	store.HandleCallback(w, r)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "unknown state")
}

func TestOIDCStore_IdPError(t *testing.T) {
	idp := fixture.NewMockIdP(t)
	store, err := New(context.Background(), Config{
		Name:        "hugr",
		IssuerURL:   idp.Srv.URL,
		ClientID:    "agent",
		RedirectURL: "http://localhost/auth/callback",
	})
	require.NoError(t, err)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet,
		"/auth/callback?error=access_denied&error_description=nope&state=hugr.x", nil)
	store.HandleCallback(w, r)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "access_denied")
}

func TestOIDCStore_NameRequired(t *testing.T) {
	_, err := New(context.Background(), Config{
		IssuerURL: "http://x",
		ClientID:  "c",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Name is required")
}
