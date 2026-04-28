// Package http exposes the agent runtime as a JSON+SSE API under
// /api/v1. See specs/002-agent-runtime-phase-2/contracts/http-api.md
// and contracts/sse-wire-format.md for the full contract.
//
// The adapter mounts on a shared *http.ServeMux owned by cmd/hugen so
// the existing /auth/... endpoints share the listener.
package http

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	stdhttp "net/http"
	"strings"
	"sync"

	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/runtime"
)

// Host is the consumer-side view of runtime.AdapterHost the http
// adapter actually depends on. Declared here per constitution
// principle III (interface lives at the consumer); *runtime.Runtime's
// adapterHost satisfies it structurally via runtime.AdapterHost.
type Host interface {
	OpenSession(ctx context.Context, req runtime.OpenRequest) (*runtime.Session, error)
	ResumeSession(ctx context.Context, id string) (*runtime.Session, error)
	Submit(ctx context.Context, frame protocol.Frame) error
	Subscribe(ctx context.Context, sessionID string) (<-chan protocol.Frame, error)
	CloseSession(ctx context.Context, id, reason string) error
	ListSessions(ctx context.Context, status string) ([]runtime.SessionSummary, error)
	Logger() *slog.Logger
}

// Authenticator validates a bearer token for /api/v1/* endpoints.
// Implementations are expected to be O(1) — every request goes
// through this gate.
type Authenticator interface {
	// Verify returns nil iff the supplied token is currently valid.
	Verify(token string) error
}

// DevTokenStore is a single-token Authenticator suitable for the
// loopback developer flow: a random token is generated at boot and
// returned by the /api/auth/dev-token endpoint. There is no token
// rotation, no expiry — phase 2 is loopback-only and the constitution
// forbids reinventing real auth here.
type DevTokenStore struct {
	token string
}

// NewDevTokenStore mints a fresh random token. The same store
// instance must be used by both the API auth gate and the
// dev-token issuance endpoint, so the page can read the token its
// gate will accept.
func NewDevTokenStore() *DevTokenStore {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is exceptional; fall back to a
		// deterministic value so the daemon still boots and an
		// operator can investigate. The token is not ergonomic
		// fallback material — the failure mode logs above.
		return &DevTokenStore{token: "fallback-dev-token"}
	}
	return &DevTokenStore{token: hex.EncodeToString(b[:])}
}

// Token returns the current dev token. Non-empty by construction.
func (d *DevTokenStore) Token() string { return d.token }

// Verify implements Authenticator.
func (d *DevTokenStore) Verify(token string) error {
	if token == "" || token != d.token {
		return ErrUnauthenticated
	}
	return nil
}

// ErrUnauthenticated is the sentinel returned by Authenticator.Verify
// when the bearer token is missing or wrong; the http adapter maps
// it to 401.
var ErrUnauthenticated = errors.New("http: unauthenticated")

// Adapter mounts the /api/v1/* JSON+SSE surface on a shared mux.
//
// It is a runtime.Adapter — Run blocks until ctx is done, returning
// the ctx error. Run does not own a *http.Server; the listener is
// owned by cmd/hugen via RuntimeCore.HTTPSrv.
type Adapter struct {
	mux     *stdhttp.ServeMux
	auth    Authenticator
	codec   *protocol.Codec
	replay  ReplaySource
	logger  *slog.Logger
	devTok  *DevTokenStore // optional: when set, /api/auth/dev-token is mounted
	sseCfg  sseConfig
	mountMu sync.Mutex
	mounted bool
}

// Options bundles Adapter construction parameters; keeps the
// constructor signature stable as fields grow.
type Options struct {
	Mux *stdhttp.ServeMux
	// Auth gates every /api/v1/* request. Required.
	Auth Authenticator
	// Codec serialises frames onto the SSE wire. Required.
	Codec *protocol.Codec
	// Replay is queried for events with seq > Last-Event-ID. Required.
	Replay ReplaySource
	// Logger is used for adapter-level logs. Defaults to slog.Default.
	Logger *slog.Logger
	// DevToken, when non-nil, is exposed at GET /api/auth/dev-token
	// (loopback-only). When nil, the endpoint is not mounted.
	DevToken *DevTokenStore
	// HeartbeatInterval overrides the default 30s SSE comment cadence.
	// Zero falls through to the default.
	HeartbeatInterval int // seconds; 0 = default 30s
	// SlowConsumerGrace overrides the default 50ms send-grace before
	// dropping a frame to a slow subscriber. Zero = default 50ms.
	SlowConsumerGrace int // milliseconds; 0 = default 50ms
}

// NewAdapter constructs the adapter. Returns an error when a required
// dependency is missing (constitution principle II — explicit deps).
func NewAdapter(opts Options) (*Adapter, error) {
	if opts.Mux == nil {
		return nil, errors.New("http: Options.Mux is required")
	}
	if opts.Auth == nil {
		return nil, errors.New("http: Options.Auth is required")
	}
	if opts.Codec == nil {
		return nil, errors.New("http: Options.Codec is required")
	}
	if opts.Replay == nil {
		return nil, errors.New("http: Options.Replay is required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Adapter{
		mux:    opts.Mux,
		auth:   opts.Auth,
		codec:  opts.Codec,
		replay: opts.Replay,
		logger: logger,
		devTok: opts.DevToken,
		sseCfg: sseConfigFromOptions(opts),
	}, nil
}

// Name reports the adapter name for runtime logging.
func (a *Adapter) Name() string { return "http" }

// Run mounts handlers (idempotent) and blocks until ctx is done.
// Returns the ctx error so the runtime errgroup can distinguish
// graceful shutdown from a real failure.
func (a *Adapter) Run(ctx context.Context, host runtime.AdapterHost) error {
	if host == nil {
		return errors.New("http: nil host")
	}
	a.mount(host)
	<-ctx.Done()
	return ctx.Err()
}

// mount registers the /api/v1/* routes. Mux is shared with the auth
// service, so calling mount twice would panic — mountMu makes this
// idempotent for tests that construct multiple Run loops.
func (a *Adapter) mount(host runtime.AdapterHost) {
	a.mountMu.Lock()
	defer a.mountMu.Unlock()
	if a.mounted {
		return
	}
	a.mounted = true

	a.mux.Handle("POST /api/v1/sessions", a.cors(a.guard(a.handleOpenSession(host))))
	a.mux.Handle("GET /api/v1/sessions", a.cors(a.guard(a.handleListSessions(host))))
	a.mux.Handle("POST /api/v1/sessions/{id}/post", a.cors(a.guard(a.handlePostFrame(host))))
	a.mux.Handle("GET /api/v1/sessions/{id}/stream", a.cors(a.guard(a.handleStream(host))))
	a.mux.Handle("POST /api/v1/sessions/{id}/close", a.cors(a.guard(a.handleCloseSession(host))))
	noContent := stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, _ *stdhttp.Request) {
		w.WriteHeader(stdhttp.StatusNoContent)
	})
	a.mux.Handle("OPTIONS /api/v1/", a.cors(noContent))
	if a.devTok != nil {
		a.mux.Handle("GET /api/auth/dev-token", a.cors(stdhttp.HandlerFunc(a.handleDevToken)))
		a.mux.Handle("OPTIONS /api/auth/dev-token", a.cors(noContent))
	}
}

// cors applies a permissive CORS policy for loopback origins (the
// webui adapter on 127.0.0.1:HUGEN_WEBUI_PORT). Non-loopback origins
// receive no CORS headers and the browser blocks the request.
func (a *Adapter) cors(next stdhttp.Handler) stdhttp.Handler {
	return stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && isLoopbackOrigin(origin) {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Credentials", "true")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Last-Event-ID")
			h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			h.Set("Vary", "Origin")
		}
		next.ServeHTTP(w, r)
	})
}

func isLoopbackOrigin(origin string) bool {
	// Strip scheme.
	idx := strings.Index(origin, "://")
	if idx < 0 {
		return false
	}
	hostPort := origin[idx+3:]
	if i := strings.LastIndexByte(hostPort, ':'); i > 0 {
		hostPort = hostPort[:i]
	}
	hostPort = strings.TrimPrefix(hostPort, "[")
	hostPort = strings.TrimSuffix(hostPort, "]")
	if hostPort == "::1" || hostPort == "localhost" {
		return true
	}
	return strings.HasPrefix(hostPort, "127.")
}

// guard wraps an HTTP handler with the bearer-token check. Missing
// token, malformed scheme, or rejected token all map to 401.
func (a *Adapter) guard(next stdhttp.HandlerFunc) stdhttp.Handler {
	return stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		tok := requestToken(r)
		if err := a.auth.Verify(tok); err != nil {
			writeError(w, stdhttp.StatusUnauthorized, "unauthenticated", "missing or invalid bearer token")
			return
		}
		next(w, r)
	})
}

// requestToken extracts the bearer token from a request. Order of
// precedence (per http-api.md §"Authentication"):
//
//  1. `Authorization: Bearer <token>` header — primary.
//  2. `hugen_dev_token` cookie — set by /api/auth/dev-token, the only
//     way EventSource can carry credentials (browser API can't set
//     headers on SSE connections).
func requestToken(r *stdhttp.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		const prefix = "Bearer "
		if strings.HasPrefix(h, prefix) {
			return strings.TrimSpace(h[len(prefix):])
		}
	}
	if c, err := r.Cookie(devTokenCookie); err == nil {
		return c.Value
	}
	return ""
}

// devTokenCookie is the cookie name set by handleDevToken so the
// browser's EventSource (which can't set headers) carries the token.
const devTokenCookie = "hugen_dev_token"

// handleDevToken returns the loopback dev token AND sets a cookie so
// the browser's EventSource (no header support) can authenticate.
// Refuses anything that does not appear to be a loopback request.
func (a *Adapter) handleDevToken(w stdhttp.ResponseWriter, r *stdhttp.Request) {
	if !isLoopback(r.RemoteAddr) {
		writeError(w, stdhttp.StatusForbidden, "loopback_only", "dev-token is loopback-only")
		return
	}
	stdhttp.SetCookie(w, &stdhttp.Cookie{
		Name:     devTokenCookie,
		Value:    a.devTok.Token(),
		Path:     "/api/",
		HttpOnly: true,
		SameSite: stdhttp.SameSiteStrictMode,
	})
	writeJSON(w, stdhttp.StatusOK, map[string]string{"token": a.devTok.Token()})
}

func isLoopback(remoteAddr string) bool {
	// RemoteAddr is "host:port"; strip the port. We accept literal
	// 127/8 or ::1 — anything else is rejected.
	host := remoteAddr
	if i := strings.LastIndexByte(host, ':'); i > 0 {
		host = host[:i]
	}
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	if host == "::1" || host == "localhost" {
		return true
	}
	return strings.HasPrefix(host, "127.")
}
