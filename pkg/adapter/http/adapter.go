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
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	stdhttp "net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/session"
)

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
// gate will accept. crypto/rand failure is treated as fatal — a
// deterministic token would silently weaken the gate.
func NewDevTokenStore() (*DevTokenStore, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	return &DevTokenStore{token: hex.EncodeToString(b[:])}, nil
}

// Token returns the current dev token. Non-empty by construction.
func (d *DevTokenStore) Token() string { return d.token }

// Verify implements Authenticator. Constant-time comparison so the
// dev token doesn't leak length/prefix information through timing.
func (d *DevTokenStore) Verify(token string) error {
	if token == "" {
		return ErrUnauthenticated
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(d.token)) != 1 {
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
// It is a session.Adapter — Run blocks until ctx is done, returning
// the ctx error. Run does not own a *http.Server; the listener is
// owned by cmd/hugen via runtime.Core.HTTPSrv.
type Adapter struct {
	mux         *stdhttp.ServeMux
	auth        Authenticator
	codec       *protocol.Codec
	replay      ReplaySource
	logger      *slog.Logger
	devTok      *DevTokenStore // optional: when set, /api/auth/dev-token is mounted
	sseCfg      sseConfig
	maxBytes    int64
	corsOrigins map[string]struct{}
	mountMu     sync.Mutex
	mountedCh   chan struct{} // closed once mount() has run; tests can wait on it.
	ready       atomic.Bool   // false until MarkReady; gates /api/v1/* with 503 runtime_starting.

	// buses is the per-session fan-out for SSE writers. The adapter
	// owns the slow-consumer drop policy so backpressure is local
	// and per-connection — see bus.go.
	busesMu sync.Mutex
	buses   map[string]*sessionBus
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
	// MaxRequestBytes caps the size of a request body the handlers
	// will read. Zero falls through to a 64 KiB default. Bodies
	// exceeding this are rejected with 413 payload_too_large.
	MaxRequestBytes int64
	// CORSAllowedOrigins is the exact set of origins the API will
	// echo back on credentialed requests. Empty disables CORS.
	// Origins are matched case-sensitively (per RFC 6454).
	CORSAllowedOrigins []string
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
	maxBytes := opts.MaxRequestBytes
	if maxBytes <= 0 {
		maxBytes = 64 << 10 // 64 KiB; the API surface is small JSON.
	}
	cors := make(map[string]struct{}, len(opts.CORSAllowedOrigins))
	for _, o := range opts.CORSAllowedOrigins {
		cors[strings.TrimRight(o, "/")] = struct{}{}
	}
	return &Adapter{
		mux:         opts.Mux,
		auth:        opts.Auth,
		codec:       opts.Codec,
		replay:      opts.Replay,
		logger:      logger,
		devTok:      opts.DevToken,
		sseCfg:      sseConfigFromOptions(opts),
		maxBytes:    maxBytes,
		corsOrigins: cors,
		mountedCh:   make(chan struct{}),
		buses:       make(map[string]*sessionBus),
	}, nil
}

// Name reports the adapter name for runtime logging.
func (a *Adapter) Name() string { return "http" }

// Mounted returns a channel that closes once the adapter has
// registered its routes on the shared mux. Tests block on it
// instead of busy-polling.
func (a *Adapter) Mounted() <-chan struct{} { return a.mountedCh }

// MarkReady flips the readiness gate so /api/v1/* requests stop
// returning 503 runtime_starting and start dispatching to handlers.
// cmd/hugen calls this once bootRuntime has produced every
// dependency the API depends on.
func (a *Adapter) MarkReady() { a.ready.Store(true) }

// Run mounts handlers (idempotent) and blocks until ctx is done.
// Returns the ctx error so the runtime errgroup can distinguish
// graceful shutdown from a real failure.
func (a *Adapter) Run(ctx context.Context, host session.AdapterHost) error {
	if host == nil {
		return errors.New("http: nil host")
	}
	a.mount(host)
	<-ctx.Done()
	return ctx.Err()
}

// mount registers the /api/v1/* routes. Mux is shared with the auth
// service, so calling mount twice would panic — mountMu makes this
// idempotent for tests that construct multiple Run loops. The
// guard against double-mount is the closed mountedCh: a closed
// channel select returns immediately, so a second mount is a
// no-op. Tests synchronise on Mounted().
func (a *Adapter) mount(host session.AdapterHost) {
	a.mountMu.Lock()
	defer a.mountMu.Unlock()
	select {
	case <-a.mountedCh:
		return // already mounted
	default:
	}
	defer close(a.mountedCh)

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

// cors echoes Access-Control-Allow-* headers iff the request Origin
// is in the explicit allowlist (Options.CORSAllowedOrigins). The
// webui adapter binds 127.0.0.1:HUGEN_WEBUI_PORT, so cmd/hugen
// passes exactly that origin; a drive-by request from any other
// loopback port gets no CORS, browser blocks it.
//
// `Vary: Origin` is set on every response, allowed or not, so a
// shared cache can't serve a CORS-permissive response to a
// request whose Origin was rejected (RFC 7234 §4.1).
func (a *Adapter) cors(next stdhttp.Handler) stdhttp.Handler {
	return stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		h := w.Header()
		h.Add("Vary", "Origin")
		origin := r.Header.Get("Origin")
		if origin != "" {
			if _, ok := a.corsOrigins[strings.TrimRight(origin, "/")]; ok {
				h.Set("Access-Control-Allow-Origin", origin)
				h.Set("Access-Control-Allow-Credentials", "true")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Last-Event-ID")
				h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			}
		}
		next.ServeHTTP(w, r)
	})
}

// guard wraps an HTTP handler with the bearer-token check. Missing
// token, malformed scheme, or rejected token all map to 401. A
// request that arrives before MarkReady is rejected with 503
// runtime_starting per http-api.md (FR-015 still gates after).
func (a *Adapter) guard(next stdhttp.HandlerFunc) stdhttp.Handler {
	return stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		if !a.ready.Load() {
			writeError(w, stdhttp.StatusServiceUnavailable, "runtime_starting",
				"agent is still starting; retry shortly")
			return
		}
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
		SameSite: stdhttp.SameSiteNoneMode,
		Secure:   true,
	})
	writeJSON(w, stdhttp.StatusOK, map[string]string{"token": a.devTok.Token()})
}

func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
