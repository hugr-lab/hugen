package fixture

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// MockIdP stands in for Keycloak / Hugr's OIDC issuer. It serves the
// discovery doc, the /authorize redirect, and the /token endpoint —
// enough surface for OIDCStore to run through a full login.
type MockIdP struct {
	Srv               *httptest.Server
	LastAuthorizeForm url.Values
	LastTokenForm     url.Values
	Access            string
	Refresh           string
	ExpiresIn         int
}

func NewMockIdP(t *testing.T) *MockIdP {
	t.Helper()
	f := &MockIdP{
		Access:    "access-token-1",
		Refresh:   "refresh-token-1",
		ExpiresIn: 300,
	}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	f.Srv = srv

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"authorization_endpoint": srv.URL + "/authorize",
			"token_endpoint":         srv.URL + "/token",
		})
	})

	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		f.LastAuthorizeForm = r.URL.Query()
		// Simulate IdP → redirect back to redirect_uri?code=…&state=…
		redirect := r.URL.Query().Get("redirect_uri")
		state := r.URL.Query().Get("state")
		http.Redirect(w, r, redirect+"?code=fake-code&state="+state, http.StatusFound)
	})

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.LastTokenForm = r.PostForm
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  f.Access,
			"refresh_token": f.Refresh,
			"expires_in":    f.ExpiresIn,
		})
	})

	return f
}
