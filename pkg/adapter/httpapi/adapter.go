// Package httpapi implements hugen's native HTTP API — the session-stateful
// agent protocol over HTTP + SSE that an external gateway (and the hub UI, and
// the A2A bridge) drives. It is the ONE interaction surface for hub-mode hugen:
// many interfaces can drive one session at once (the runtime's multi-subscriber
// fanout), reads stream over SSE and writes are discrete POSTs.
//
// It is a manager.Adapter sibling of pkg/adapter/tui, mounted by the
// `hugen serve` run mode. See design/008-integration/spec-http-api.md.
//
// This file is the H1 skeleton: the agent card (/v1/agent), health probes, the
// two listener modes (shared auth mux vs a dedicated port), and the fail-closed
// boot gate (D4). Forwarded-user-token identity (H2) and the session surface
// (H3–H6) land next.
package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/hugr-lab/hugen/pkg/adapter"
)

const (
	// cardPath serves the agent card (name / capabilities / API descriptor).
	// Public — discovery needs no auth, like the A2A well-known card.
	cardPath = "/v1/agent"
	// whoamiPath returns the verified end-user identity (protected). H2.
	whoamiPath = "/v1/whoami"
	// healthzPath / readyzPath are k8s probes (dedicated-listener mode only).
	healthzPath = "/healthz"
	readyzPath  = "/readyz"

	defaultAgentName = "hugen"
	defaultAgentDesc = "Hugr Data Mesh AI analyst — query, explore, and report over the Hugr catalog."

	// agentVersion is the card's own version string. Bump as the surface grows.
	agentVersion = "0.1.0"
)

// Adapter is the native HTTP API adapter. It implements manager.Adapter (via
// the pkg/adapter alias) and is wired into the runtime's adapter slice exactly
// like the TUI and A2A adapters.
type Adapter struct {
	logger  *slog.Logger
	baseURL string // public base URL the card advertises (gateway-facing)

	// Listener mode (mutually exclusive), chosen by the cmd layer from
	// HUGEN_API_PORT: sharedMux → mount on the runtime's auth listener;
	// listenPort → a dedicated http.Server. Dedicated is the norm (one
	// container = one agent = its own port).
	sharedMux  *http.ServeMux
	listenPort int

	agentName string
	agentDesc string

	// issuer is the hub OIDC issuer whose signature forwarded user tokens are
	// verified against (D1 — the SAME key hugr uses, reused not new). Empty +
	// !allowOpen ⇒ Run fails closed (D4): the session surface can't authenticate
	// anyone without it.
	issuer    string
	allowOpen bool
	// devUI serves the built-in browser dev client at /ui. Off by default —
	// opt-in via HUGEN_API_DEV_UI. It uses EventSource (no auth header), so it
	// is only useful on an allow-open endpoint; never expose it on a real one.
	devUI bool

	// verify authenticates a forwarded user token (H2). nil ⇒ allow-open dev
	// (authMiddleware injects devUser). Set via WithVerifier from the cmd layer,
	// which owns the hugr coupling.
	verify VerifyFunc

	// artifacts backs the H6 artifact endpoints. nil ⇒ artifact endpoints
	// return 501. Wired from core.Artifacts in the cmd layer.
	artifacts ArtifactStore

	host adapter.Host
	// lifecycleCtx is the adapter's Run ctx (process lifetime). Sessions are
	// opened/closed on it, NOT a per-request ctx — a session's Run loop must
	// outlive any single HTTP request (the same discipline as the a2a adapter).
	lifecycleCtx context.Context
}

// Option configures an Adapter.
type Option func(*Adapter)

// WithLogger sets the adapter logger (defaults to host.Logger() in Run).
func WithLogger(l *slog.Logger) Option { return func(a *Adapter) { a.logger = l } }

// WithBaseURL sets the public base URL the card advertises.
func WithBaseURL(u string) Option { return func(a *Adapter) { a.baseURL = u } }

// WithSharedMux selects shared-listener mode: mount on the supplied mux (the
// runtime's auth/callback mux) and rely on its already-running http.Server.
func WithSharedMux(m *http.ServeMux) Option { return func(a *Adapter) { a.sharedMux = m } }

// WithListenPort selects dedicated-listener mode on the given port. Ignored
// when WithSharedMux is also set.
func WithListenPort(p int) Option { return func(a *Adapter) { a.listenPort = p } }

// WithAgentIdentity overrides the card's name/description.
func WithAgentIdentity(name, desc string) Option {
	return func(a *Adapter) {
		if name != "" {
			a.agentName = name
		}
		if desc != "" {
			a.agentDesc = desc
		}
	}
}

// WithIssuer sets the hub OIDC issuer used to verify forwarded user tokens
// (H2). When empty, the endpoint is unauthenticated and Run fails closed unless
// WithAllowOpen is also set.
func WithIssuer(url string) Option { return func(a *Adapter) { a.issuer = url } }

// WithAllowOpen permits serving with no issuer configured (local dev). Without
// it, Run fails closed when no issuer is set (D4).
func WithAllowOpen(v bool) Option { return func(a *Adapter) { a.allowOpen = v } }

// WithDevUI serves the built-in browser dev client at /ui. Off by default
// (HUGEN_API_DEV_UI). It has no auth (EventSource), so enable it only on an
// allow-open dev endpoint.
func WithDevUI(v bool) Option { return func(a *Adapter) { a.devUI = v } }

// WithVerifier installs the forwarded-user-token verifier (H2). Without it the
// endpoint runs in allow-open dev mode — every request is the local dev user.
func WithVerifier(f VerifyFunc) Option { return func(a *Adapter) { a.verify = f } }

// WithArtifactStore enables the H6 artifact endpoints (list / download /
// ingest). nil leaves them returning 501.
func WithArtifactStore(s ArtifactStore) Option { return func(a *Adapter) { a.artifacts = s } }

// New constructs the HTTP API adapter. Callers select a listener mode via
// WithSharedMux or WithListenPort; New defaults neither (the cmd layer decides
// from HUGEN_API_PORT).
func New(opts ...Option) *Adapter {
	a := &Adapter{
		logger:    slog.Default(),
		agentName: defaultAgentName,
		agentDesc: defaultAgentDesc,
	}
	for _, o := range opts {
		o(a)
	}
	return a
}

// Name implements manager.Adapter.
func (a *Adapter) Name() string { return "httpapi" }

// Run implements manager.Adapter. It mounts the card + health probes and serves
// until ctx cancels. Shared mode registers handlers on the runtime's mux and
// blocks on ctx; dedicated mode owns an http.Server on listenPort and shuts it
// down gracefully.
func (a *Adapter) Run(ctx context.Context, host adapter.Host) error {
	a.host = host
	a.lifecycleCtx = ctx
	if a.logger == nil {
		a.logger = host.Logger()
	}

	// Fail closed (D4): the session surface (H2+) authenticates forwarded user
	// tokens against the hub issuer. With no issuer we cannot verify anyone, so
	// refuse to serve unless the operator explicitly opts into an open endpoint.
	if a.issuer == "" && !a.allowOpen {
		return errors.New("httpapi: refusing to serve with no token issuer — set HUGR_ISSUER, or HUGEN_API_ALLOW_OPEN=1 to allow an unauthenticated endpoint explicitly")
	}
	if a.issuer == "" {
		a.logger.Warn("httpapi: NO issuer — endpoint is UNAUTHENTICATED (HUGEN_API_ALLOW_OPEN set); do NOT expose it")
	}

	if a.sharedMux != nil {
		if err := a.mount(a.sharedMux, false); err != nil {
			return err
		}
		a.logger.Info("httpapi: mounted on shared auth listener", "card", cardPath, "base_url", a.baseURL)
		<-ctx.Done()
		return nil
	}

	mux := http.NewServeMux()
	if err := a.mount(mux, true); err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", a.listenPort),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			a.logger.Warn("httpapi: dedicated listener shutdown", "err", err)
		}
	}()
	a.logger.Info("httpapi: dedicated listener", "addr", srv.Addr, "card", cardPath)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// mount registers the H1 handlers on mux. Health probes are dedicated-listener
// only (the shared auth listener has its own; registering them there would
// collide on the mux pattern).
func (a *Adapter) mount(mux *http.ServeMux, health bool) error {
	cardBytes, err := a.marshalCard()
	if err != nil {
		return fmt.Errorf("httpapi: marshal card: %w", err)
	}
	mux.HandleFunc(cardPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(cardBytes)
	})
	// Protected routes go behind authMiddleware (verify forwarded user token →
	// identity in ctx). Card + health stay public.
	mux.Handle(whoamiPath, a.authMiddleware(http.HandlerFunc(whoamiHandler)))
	// H3: session lifecycle. Owner-scoped — a caller sees/controls only its own
	// sessions (identified by the httpapi_owner metadata stamp).
	mux.Handle("POST /v1/sessions", a.authMiddleware(http.HandlerFunc(a.handleCreateSession)))
	mux.Handle("GET /v1/sessions", a.authMiddleware(http.HandlerFunc(a.handleListSessions)))
	mux.Handle("GET /v1/sessions/{id}", a.authMiddleware(http.HandlerFunc(a.handleGetSession)))
	mux.Handle("DELETE /v1/sessions/{id}", a.authMiddleware(http.HandlerFunc(a.handleDeleteSession)))
	// H4: write path — drive the session by submitting inbound frames.
	mux.Handle("POST /v1/sessions/{id}/messages", a.authMiddleware(http.HandlerFunc(a.handleSendMessage)))
	mux.Handle("POST /v1/sessions/{id}/inquiry", a.authMiddleware(http.HandlerFunc(a.handleInquiryResponse)))
	mux.Handle("POST /v1/sessions/{id}/cancel", a.authMiddleware(http.HandlerFunc(a.handleCancel)))
	// H5: the SSE frame stream — replay + live, multi-subscriber.
	mux.Handle("GET /v1/sessions/{id}/stream", a.authMiddleware(http.HandlerFunc(a.handleStream)))
	// H6: history + artifacts.
	mux.Handle("GET /v1/sessions/{id}/events", a.authMiddleware(http.HandlerFunc(a.handleListEvents)))
	mux.Handle("GET /v1/sessions/{id}/artifacts", a.authMiddleware(http.HandlerFunc(a.handleListArtifacts)))
	mux.Handle("GET /v1/sessions/{id}/artifacts/{aid}", a.authMiddleware(http.HandlerFunc(a.handleGetArtifact)))
	mux.Handle("POST /v1/sessions/{id}/artifacts", a.authMiddleware(http.HandlerFunc(a.handleIngestArtifact)))
	if health {
		mux.HandleFunc(healthzPath, healthHandler)
		mux.HandleFunc(readyzPath, healthHandler)
	}
	// H9: the minimal dev client — OFF by default, opt-in via HUGEN_API_DEV_UI.
	// It uses EventSource (no auth header), so it is only useful on an allow-open
	// endpoint; never enable it on a real one.
	if a.devUI {
		mux.HandleFunc(uiPath, serveUI)
		a.logger.Warn("httpapi: dev client SERVED at /ui (HUGEN_API_DEV_UI) — dev only, unauthenticated", "path", uiPath)
	}
	return nil
}

// healthHandler is a liveness/readiness probe: 200 OK, plain body.
func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
