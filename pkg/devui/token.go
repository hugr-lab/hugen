// Package devui exposes the developer-facing UI surface: the ADK webui
// SPA, the ADK REST API, and small /dev helpers for copy-pasting an
// access token into curl / Postman without clicking through the OIDC
// flow manually.
//
// The devui listener binds to 127.0.0.1 only (loopback). Prod/A2A
// listeners stay in pkg/a2a. The caller wires both listeners from
// cmd/agent/main.go so the authentication redirect_uri is independent
// of run mode.
package devui

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/hugr-lab/hugen/pkg/auth"
)

// TokenResponse is what GET /dev/token?name=<auth-name> returns on
// success. Mirrors OAuth-style bearer-token shape so the output is
// paste-ready for `Authorization: Bearer …` headers.
type TokenResponse struct {
	Name        string `json:"name"`
	AccessToken string `json:"access_token"`
	Type        string `json:"type"`
}

// TokenHandler returns a handler that serves /dev/token?name=<auth-name>.
// It reads the access token from the configured TokenStore, gated to
// loopback callers only (RemoteAddr 127.0.0.1 / ::1).
//
// auths is auth.SourceRegistry.TokenStores() — keyed by the auth
// name the operator wrote in config.yaml.
func TokenHandler(auths map[string]auth.TokenStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopback(r.RemoteAddr) {
			http.Error(w, "forbidden: loopback only", http.StatusForbidden)
			return
		}
		name := r.URL.Query().Get("name")
		if name == "" {
			// Default to the first entry when the operator has a single
			// auth — common dev-loop shape.
			for n := range auths {
				if name != "" {
					// >1 → ambiguous, require explicit name.
					http.Error(w, "missing ?name= (multiple auth configs)", http.StatusBadRequest)
					return
				}
				name = n
			}
		}
		store, ok := auths[name]
		if !ok || store == nil {
			http.Error(w, fmt.Sprintf("unknown auth %q", name), http.StatusNotFound)
			return
		}
		token, err := store.Token(r.Context())
		if err != nil {
			http.Error(w, fmt.Sprintf("token fetch failed: %v", err), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(TokenResponse{
			Name: name, AccessToken: token, Type: "Bearer",
		})
	})
}

// TriggerAuthHandler returns a handler for /dev/auth/trigger?name=<auth-name>
// that 302-redirects the caller to the A2A listener's /auth/<name>/login
// route, re-triggering the OIDC login flow when the cached token has
// expired. a2aBaseURL is the externally-visible A2A listener base URL
// (e.g. "http://localhost:10000").
func TriggerAuthHandler(a2aBaseURL string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopback(r.RemoteAddr) {
			http.Error(w, "forbidden: loopback only", http.StatusForbidden)
			return
		}
		name := r.URL.Query().Get("name")
		if name == "" {
			http.Error(w, "missing ?name=", http.StatusBadRequest)
			return
		}
		target := strings.TrimRight(a2aBaseURL, "/") + "/auth/" + name + "/login"
		http.Redirect(w, r, target, http.StatusFound)
	})
}

// isLoopback inspects RemoteAddr and returns true only when the client
// connected via 127.0.0.1 / ::1. Does NOT trust X-Forwarded-For.
func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
