package auth

import (
	"fmt"
	"log/slog"
	"net/http"
)

// Transport returns an http.RoundTripper that injects a Bearer token
// from the given TokenStore into every outgoing request.
func Transport(store TokenStore, base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &tokenTransport{store: store, base: base}
}

// HeaderTransport returns an http.RoundTripper that injects a static
// header (e.g. `X-API-Key: ${WEATHER_API_KEY}`) into every outgoing
// request. The value is copied once at construction time — env
// expansion happens in the caller before passing it in. Used for MCP
// providers that authenticate via a non-Bearer scheme.
func HeaderTransport(headerName, headerValue string, base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &headerTransport{name: headerName, value: headerValue, base: base}
}

type tokenTransport struct {
	store TokenStore
	base  http.RoundTripper
}

func (t *tokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	token, err := t.store.Token(req.Context())
	if err != nil {
		return nil, fmt.Errorf("get auth token: %w", err)
	}
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+token)
	resp, err := t.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		slog.Warn("hugr auth rejected", "status", resp.StatusCode, "url", req.URL.String())
	}
	return resp, nil
}

type headerTransport struct {
	name  string
	value string
	base  http.RoundTripper
}

func (h *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set(h.name, h.value)
	return h.base.RoundTrip(r)
}
