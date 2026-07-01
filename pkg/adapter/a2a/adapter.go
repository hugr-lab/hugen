// Package a2a implements the A2A (Agent2Agent) protocol adapter — the
// integration surface that makes hugen reachable as a first-class agent
// from Microsoft Teams / Copilot and any spec-compliant A2A client.
//
// It is a sibling of pkg/adapter/tui: a manager.Adapter whose Run hosts
// an A2A server (agent card + JSON-RPC/SSE) built on the official
// github.com/a2aproject/a2a-go/v2 SDK (a2asrv), with NO ADK. Stage 1
// of design/008-integration; see spec-a2a-adapter.md.
//
// This file is the A1 skeleton: agent-card hosting, the JSON-RPC
// transport mount, and the two listener modes (shared auth mux vs a
// dedicated port). The AgentExecutor here is a trivial echo (executor.go);
// the real contextId-session ↔ Frame translation lands in A2–A6.
package a2a

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2acompat/a2av0"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/hugr-lab/hugen/pkg/adapter"
	"github.com/hugr-lab/hugen/pkg/protocol"
)

const (
	// jsonRPCPath is where the A2A JSON-RPC (+SSE) transport mounts.
	// The agent card advertises baseURL+jsonRPCPath as the interface URL.
	jsonRPCPath = "/a2a"

	// legacyCardPath is the pre-v0.2.5 well-known location. Microsoft's
	// Copilot auto-pull references this legacy path, so we alias the
	// canonical a2asrv.WellKnownAgentCardPath onto it. (a2a-integration.md §1.4.)
	legacyCardPath = "/.well-known/agent.json"

	defaultAgentName = "hugen"
	defaultAgentDesc = "Hugr Data Mesh AI analyst — query, explore, and report over the Hugr catalog."
	defaultSkillID   = "hugr-analyst"

	// agentVersion is the card's own version string (distinct from the A2A
	// protocol version). Strict clients (a2a-inspector / a2a-tck, A11) expect
	// a non-empty value. Bump as the adapter surface evolves.
	agentVersion = "0.1.0"

	// apiKeyHeader is the request header the API-key gate reads (A9). It is the
	// header name the card advertises, so a client (e.g. a Copilot custom
	// connector's API-key auth) knows where to put the key.
	apiKeyHeader = "X-API-Key"

	// apiKeySchemeName names the apiKey security scheme in the agent card.
	apiKeySchemeName = "apiKey"
)

// Adapter is the A2A protocol adapter. It implements manager.Adapter
// (via the pkg/adapter alias) and is wired into the runtime's adapter
// slice exactly like the TUI adapter.
type Adapter struct {
	logger  *slog.Logger
	baseURL string // public URL the agent card advertises (tunnel hostname in prod)

	// Listener mode (mutually exclusive):
	//   - sharedMux != nil → mount on the runtime's existing auth/callback
	//     listener; Run registers handlers and blocks on ctx (the runtime
	//     already serves). HUGEN_A2A_PORT=0.
	//   - sharedMux == nil → bind a dedicated http.Server on listenPort.
	//     Recommended for tunnel-exposed runs (spec §6.1 loopback caveat).
	sharedMux  *http.ServeMux
	listenPort int

	agentName string
	agentDesc string

	// owner is the service identity that owns A2A-opened root sessions
	// (single identity for v1; A9 auth may override). Defaults to
	// serviceParticipant().
	owner protocol.ParticipantInfo

	// apiKey gates the JSON-RPC endpoint (A9). When non-empty, every /a2a
	// request must carry it in the apiKeyHeader header or gets 401, and the
	// card advertises the apiKey security scheme so clients know to send it.
	// Empty = open endpoint (logged loud). The authenticated principal still
	// maps to the single service identity in v1 — the key gates access, it
	// does not select a per-user identity (that is Stage 2 / hub OBO). Set
	// from HUGEN_A2A_API_KEY (an adapter knob → env, never YAML).
	apiKey string

	// allowOpen lets the adapter serve an unauthenticated /a2a endpoint. Without
	// it (the default) Run REFUSES to serve open — fail-closed (H2), because an
	// open endpoint with no session reaper (A8, deferred) is a trivial resource
	// -exhaustion vector (any contextId opens a permanent root). Set via
	// HUGEN_A2A_ALLOW_OPEN=1 for a throwaway local run; never over a tunnel.
	allowOpen bool

	// artifactResolve resolves a published artifact (rootID, id) to a local
	// path so the by-ref download endpoint (A10) can stream it. nil disables
	// artifact delivery (no /a2a/artifacts/ endpoint, no FilePart emitted).
	// Wired from core.Artifacts.Store().Path in runA2A.
	artifactResolve artifactResolver

	// host is the runtime side of the adapter contract, captured in Run.
	host adapter.Host
	// reg maps contextIds to durable root sessions (A2). Built in Run; the
	// executor resolves through it and Cancel forgets through it. NOTE: there is
	// no idle-GC yet (A8, deferred) — every distinct contextId opens a permanent
	// root, so the endpoint must stay private/keyed until A8 lands (H1/H2).
	reg *contextRegistry
}

// Option configures the Adapter.
type Option func(*Adapter)

func WithLogger(l *slog.Logger) Option { return func(a *Adapter) { a.logger = l } }

// WithBaseURL sets the public URL the agent card advertises (the value a
// client dials). In a tunnel deployment this is the tunnel hostname.
func WithBaseURL(u string) Option { return func(a *Adapter) { a.baseURL = u } }

// WithSharedMux selects shared-listener mode: the adapter mounts its
// handlers on the supplied mux (the runtime's auth/callback mux) and
// relies on the runtime's already-running http.Server to serve them.
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

// WithServiceIdentity overrides the service identity that owns A2A-opened
// root sessions. Empty fields keep the default (serviceParticipant()).
func WithServiceIdentity(id, name string) Option {
	return func(a *Adapter) {
		if id != "" {
			a.owner.ID = id
		}
		if name != "" {
			a.owner.Name = name
		}
	}
}

// WithAPIKey gates the JSON-RPC endpoint behind an API key carried in the
// apiKeyHeader header (A9). Empty leaves the endpoint open (see WithAllowOpen).
func WithAPIKey(key string) Option { return func(a *Adapter) { a.apiKey = strings.TrimSpace(key) } }

// WithAllowOpen permits serving an unauthenticated /a2a endpoint. Without it,
// Run fails closed when no API key is set (H2).
func WithAllowOpen(v bool) Option { return func(a *Adapter) { a.allowOpen = v } }

// WithArtifactResolver enables by-ref artifact delivery (A10): published
// artifacts surface as A2A FileParts pointing at a signed download URL served
// by the adapter, and r resolves (rootID, id) → local path. Nil = disabled.
func WithArtifactResolver(r artifactResolver) Option {
	return func(a *Adapter) { a.artifactResolve = r }
}

// New constructs an A2A adapter. Callers must select a listener mode via
// WithSharedMux or WithListenPort; New does not default one (the cmd layer
// decides from HUGEN_A2A_PORT).
func New(opts ...Option) *Adapter {
	a := &Adapter{
		logger:    slog.Default(),
		agentName: defaultAgentName,
		agentDesc: defaultAgentDesc,
		owner:     serviceParticipant(),
	}
	for _, o := range opts {
		o(a)
	}
	return a
}

// Name implements manager.Adapter.
func (a *Adapter) Name() string { return "a2a" }

// Run implements manager.Adapter. It builds the a2asrv handler stack,
// mounts the agent card (+ legacy alias) and the JSON-RPC transport, and
// serves until ctx cancels.
//
// In shared mode the runtime's http.Server already serves the mux, so Run
// just registers handlers and blocks on ctx. In dedicated mode Run owns an
// http.Server on listenPort and shuts it down gracefully on ctx cancel.
func (a *Adapter) Run(ctx context.Context, host adapter.Host) error {
	a.host = host
	if a.logger == nil {
		a.logger = host.Logger()
	}
	// The registry opens/resumes durable roots on the adapter's Run ctx (the
	// whole process lifetime), NOT a per-request ctx — a session's Run loop
	// must outlive any single A2A request.
	a.reg = newContextRegistry(hostRootStore{host: host, owner: a.owner, lifecycleCtx: ctx}, a.logger)

	// A10: artifact delivery. The signing secret is the API key when set
	// (ties artifact access to the same trust) else a fresh random one; the
	// executor stamps the by-ref signed URL on every published artifact's
	// FilePart, and the /a2a/artifacts/ endpoint verifies + serves it.
	var artifactURL func(rootID, id string) string
	artifactSecret := a.apiKey
	if a.artifactResolve != nil {
		if artifactSecret == "" {
			// L1: fail closed — never fall back to a known constant secret (which
			// would make every signed URL forgeable). If crypto/rand is
			// unavailable (effectively never), disable artifact delivery.
			s, err := randomArtifactSecret()
			if err != nil {
				a.logger.Warn("a2a: artifact delivery DISABLED — no signing secret", "err", err)
				a.artifactResolve = nil
			} else {
				artifactSecret = s
			}
		}
	}
	if a.artifactResolve != nil {
		base := a.baseURL
		artifactURL = func(rootID, id string) string {
			return signedArtifactURL(base, artifactSecret, rootID, id, time.Now())
		}
		a.logger.Info("a2a: by-ref artifact delivery enabled", "endpoint", artifactPathPrefix)
	}

	card := a.buildCard()
	handler := a2asrv.NewHandler(newSessionExecutor(a.logger, a.reg, host, a.owner, artifactURL))
	// Serve BOTH wire versions over the same RequestHandler, dispatched by the
	// A2A-Version header (A4): Microsoft Copilot posts message/send (v0.3) with
	// no version header, while spec-compliant v1.0 clients send "1.x". Without
	// the v0.3 leg Copilot can't reach the agent at all.
	var rpc http.Handler = versionDispatchHandler{
		v1:  a2asrv.NewJSONRPCHandler(handler),
		v03: a2av0.NewJSONRPCHandler(handler),
	}
	// A9: gate the RPC endpoint behind the API key (the card stays open so
	// clients can discover the requirement). An open endpoint is logged loud.
	if a.apiKey != "" {
		rpc = a.apiKeyGate(rpc)
		a.logger.Info("a2a: API-key auth enabled", "header", apiKeyHeader)
	} else if a.allowOpen {
		a.logger.Warn("a2a: NO API key — JSON-RPC endpoint is OPEN (HUGEN_A2A_ALLOW_OPEN set); do NOT expose it over a tunnel — no session reaper yet (A8)")
	} else {
		// Fail closed (H2): an open endpoint with no idle-GC (A8) is a trivial
		// resource-exhaustion vector — any contextId opens a permanent root.
		return fmt.Errorf("a2a: refusing to serve an OPEN JSON-RPC endpoint — set HUGEN_A2A_API_KEY, or HUGEN_A2A_ALLOW_OPEN=1 to allow it explicitly")
	}
	// Serve a DUAL-shaped card, not a2asrv.NewStaticAgentCardHandler (which
	// emits the v1.0-only shape). The a2a-go/v2 AgentCard carries just
	// `supportedInterfaces`; v0.3 consumers (a2a-inspector, Microsoft Copilot)
	// validate against the legacy schema and hard-reject a card with no
	// top-level `url`. Carrying both shapes makes the card acceptable to either.
	cardBytes, cardErr := marshalDualCard(card, a.baseURL+jsonRPCPath)
	if cardErr != nil {
		return fmt.Errorf("a2a: marshal card: %w", cardErr)
	}
	cardHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(cardBytes)
	})

	register := func(mux *http.ServeMux) {
		mux.Handle(a2asrv.WellKnownAgentCardPath, cardHandler)
		mux.Handle(legacyCardPath, cardHandler)
		mux.Handle(jsonRPCPath, rpc)
		// A10: by-ref artifact download, self-authenticated by the signed URL
		// (NOT behind the API-key header gate).
		if a.artifactResolve != nil {
			mux.Handle(artifactPathPrefix, artifactDownloadHandler(artifactSecret, a.artifactResolve, a.logger))
		}
	}

	if a.sharedMux != nil {
		register(a.sharedMux)
		a.logger.Info("a2a: mounted on shared auth listener",
			"card", a2asrv.WellKnownAgentCardPath, "rpc", jsonRPCPath, "base_url", a.baseURL)
		<-ctx.Done()
		return nil
	}

	mux := http.NewServeMux()
	register(mux)
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
			a.logger.Warn("a2a: dedicated listener shutdown", "err", err)
		}
	}()
	a.logger.Info("a2a: dedicated listener started",
		"addr", srv.Addr, "card", a2asrv.WellKnownAgentCardPath, "rpc", jsonRPCPath, "base_url", a.baseURL)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("a2a: listen: %w", err)
	}
	return nil
}

// protocolVersion03 is the v0.3 wire version string (the SDK exports a const
// only for the current 1.0). The same /a2a endpoint serves both via the
// version-dispatch handler, so the card advertises both interfaces.
const protocolVersion03 a2a.ProtocolVersion = "0.3"

// versionDispatchHandler routes A2A JSON-RPC at /a2a by the A2A-Version header:
// "1.x" → the v1.0 handler; empty or "0.3" → the v0.3 compat handler. Per the
// A2A spec an absent header means 0.3 — which is exactly what Microsoft Copilot
// sends (it posts message/send with no version header).
type versionDispatchHandler struct {
	v1  http.Handler
	v03 http.Handler
}

func (h versionDispatchHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(strings.TrimSpace(r.Header.Get("A2A-Version")), "1.") {
		h.v1.ServeHTTP(w, r)
		return
	}
	h.v03.ServeHTTP(w, r)
}

// apiKeyGate wraps next so every request must carry the configured key in the
// apiKeyHeader header (constant-time compared) — otherwise 401 (A9). Only
// reached when a.apiKey != "". The agent card is served outside this gate so
// clients can still discover the auth requirement.
func (a *Adapter) apiKeyGate(next http.Handler) http.Handler {
	want := []byte(a.apiKey)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimSpace(r.Header.Get(apiKeyHeader))
		if subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			w.Header().Set("WWW-Authenticate", apiKeySchemeName)
			http.Error(w, "unauthorized: missing or invalid API key", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// buildCard assembles the v1.0 agent card. Stage 1 advertises one skill
// ("hugr analyst") over the JSON-RPC interface; missions/tasks map to
// additional AgentSkills later (spec §1 non-goals). The same /a2a endpoint
// serves v1.0 and v0.3 (header-dispatched), so both are advertised.
func (a *Adapter) buildCard() *a2a.AgentCard {
	url := a.baseURL + jsonRPCPath
	ifaceV1 := a2a.NewAgentInterface(url, a2a.TransportProtocolJSONRPC)
	ifaceV1.ProtocolVersion = a2a.Version // "1.0"
	ifaceV03 := a2a.NewAgentInterface(url, a2a.TransportProtocolJSONRPC)
	ifaceV03.ProtocolVersion = protocolVersion03
	card := &a2a.AgentCard{
		Name:                a.agentName,
		Description:         a.agentDesc,
		Version:             agentVersion,
		SupportedInterfaces: []*a2a.AgentInterface{ifaceV1, ifaceV03},
		Capabilities: a2a.AgentCapabilities{
			Streaming:         true,
			PushNotifications: true,
		},
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		Skills: []a2a.AgentSkill{{
			ID:          defaultSkillID,
			Name:        "Hugr analyst",
			Description: "Ask questions over the Hugr data mesh: explore the catalog, run GraphQL queries, build reports.",
			Tags:        []string{"data", "analytics", "hugr", "sql"},
			Examples: []string{
				"What tables are available in the catalog?",
				"Summarise revenue by region for the last quarter.",
			},
		}},
	}
	// A9: advertise the apiKey security scheme + require it, so a client knows
	// to send the key in apiKeyHeader. Only when a key is actually configured —
	// an open endpoint advertises no security.
	if a.apiKey != "" {
		card.SecuritySchemes = a2a.NamedSecuritySchemes{
			apiKeySchemeName: a2a.APIKeySecurityScheme{
				Description: "Static API key issued by the operator.",
				Location:    a2a.APIKeySecuritySchemeLocationHeader,
				Name:        apiKeyHeader,
			},
		}
		card.SecurityRequirements = a2a.SecurityRequirementsOptions{
			{apiKeySchemeName: a2a.SecuritySchemeScopes{}},
		}
	}
	return card
}

// dualAgentCard wraps the v1.0 AgentCard (which the a2a-go/v2 type emits with
// only `supportedInterfaces`) and adds the legacy top-level fields a v0.3
// consumer's schema requires — `url`, `preferredTransport`, `protocolVersion`.
// The embedded card's fields are promoted in JSON, so the served card satisfies
// BOTH a v1.0 client (reads supportedInterfaces) and a v0.3 client / validator
// (a2a-inspector, Microsoft Copilot — reads the top-level url). A9.
type dualAgentCard struct {
	*a2a.AgentCard
	URL                string              `json:"url"`
	PreferredTransport string              `json:"preferredTransport"`
	ProtocolVersion    a2a.ProtocolVersion `json:"protocolVersion"`
}

// marshalDualCard serves the both-shapes card JSON. rpcURL is the JSON-RPC
// endpoint (baseURL + jsonRPCPath).
func marshalDualCard(card *a2a.AgentCard, rpcURL string) ([]byte, error) {
	return json.Marshal(dualAgentCard{
		AgentCard:          card,
		URL:                rpcURL,
		PreferredTransport: string(a2a.TransportProtocolJSONRPC),
		ProtocolVersion:    protocolVersion03,
	})
}
