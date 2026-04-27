package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeIdP stands in for Keycloak / Hugr's OIDC issuer. It serves the
// discovery doc, the /authorize redirect, and the /token endpoint —
// enough surface for OIDCStore to run through a full login.
type fakeIdP struct {
	srv               *httptest.Server
	lastAuthorizeForm url.Values
	lastTokenForm     url.Values
	access            string
	refresh           string
	expiresIn         int
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	f := &fakeIdP{
		access:    "access-token-1",
		refresh:   "refresh-token-1",
		expiresIn: 300,
	}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	f.srv = srv

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"authorization_endpoint": srv.URL + "/authorize",
			"token_endpoint":         srv.URL + "/token",
		})
	})

	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		f.lastAuthorizeForm = r.URL.Query()
		// Simulate IdP → redirect back to redirect_uri?code=…&state=…
		redirect := r.URL.Query().Get("redirect_uri")
		state := r.URL.Query().Get("state")
		http.Redirect(w, r, redirect+"?code=fake-code&state="+state, http.StatusFound)
	})

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.lastTokenForm = r.PostForm
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  f.access,
			"refresh_token": f.refresh,
			"expires_in":    f.expiresIn,
		})
	})

	return f
}

func TestOIDCStore_StatePrefixAndLoginCallback(t *testing.T) {
	idp := newFakeIdP(t)

	// Our RedirectURL points back at ourselves; we'll hand-fire the
	// callback through SourceRegistry rather than over HTTP.
	redirect := "http://localhost:10000/auth/callback"
	store, err := NewOIDCStore(context.Background(), OIDCConfig{
		Name:        "hugr",
		IssuerURL:   idp.srv.URL,
		ClientID:    "agent",
		RedirectURL: redirect,
	})
	require.NoError(t, err)

	reg := NewSourceRegistry(nil)
	require.NoError(t, reg.Add(store))
	mux := http.NewServeMux()
	reg.Mount(mux)

	// Step 1: hit /auth/login/hugr — should redirect to /authorize
	// with state prefixed by the source name.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/auth/login/hugr", nil)
	mux.ServeHTTP(w, r)
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

	// Hand-fire the /auth/callback through the shared mux.
	cw := httptest.NewRecorder()
	cr := httptest.NewRequest(http.MethodGet, "/auth/callback?code="+code+"&state="+state, nil)
	mux.ServeHTTP(cw, cr)
	require.Equalf(t, http.StatusOK, cw.Code, "callback body: %s", cw.Body.String())
	assert.Contains(t, cw.Body.String(), "Login successful")

	// Step 3: Token() now returns the access token without blocking.
	tok, err := store.Token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "access-token-1", tok)

	// Token endpoint received an authorization_code exchange.
	assert.Equal(t, "authorization_code", idp.lastTokenForm.Get("grant_type"))
	assert.Equal(t, "fake-code", idp.lastTokenForm.Get("code"))
}

func TestOIDCStore_UnknownStateRejected(t *testing.T) {
	idp := newFakeIdP(t)
	store, err := NewOIDCStore(context.Background(), OIDCConfig{
		Name:        "hugr",
		IssuerURL:   idp.srv.URL,
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
	idp := newFakeIdP(t)
	store, err := NewOIDCStore(context.Background(), OIDCConfig{
		Name:        "hugr",
		IssuerURL:   idp.srv.URL,
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
	_, err := NewOIDCStore(context.Background(), OIDCConfig{
		IssuerURL: "http://x",
		ClientID:  "c",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Name is required")
}
