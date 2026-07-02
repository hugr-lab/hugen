// Package a2a implements the A2A (Agent2Agent) protocol bridge — the
// integration surface that makes hugen reachable as a first-class agent
// from Microsoft Teams / Copilot and any spec-compliant A2A client.
//
// It is a STANDALONE service (cmd/a2a), NOT an in-runtime adapter: Server
// hosts the A2A surface (agent card + JSON-RPC/SSE, on the a2a-go/v2 SDK,
// NO ADK) and drives hugen through the native HTTP API (pkg/hugenclient)
// over HTTP. The contextId↔session translation, inquiry parking, async-Task
// handling, and card live here; the I/O seam is hugenclient (clientRootStore
// + clientFrameIO), which is what lets the bridge run out-of-process and
// keeps a2a-go out of the hugen core. design/008-integration/spec-http-api.md
// (H8) + spec-a2a-adapter.md.
package a2a

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2acompat/a2av0"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/hugr-lab/hugen/pkg/hugenclient"
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

// Server is the standalone A2A bridge: it hosts the A2A protocol surface
// (agent card + JSON-RPC/SSE) and drives hugen through the native HTTP API
// (hugenclient) — out-of-process, NOT an in-runtime adapter. It owns a
// dedicated http.Server on listenPort.
type Server struct {
	logger  *slog.Logger
	baseURL string // public URL the agent card advertises (tunnel hostname in prod)

	listenPort int

	agentName string
	agentDesc string

	// owner is the service identity label on A2A-opened root sessions. The
	// actual session owner is set API-side from the bridge's token; this is
	// retained for frame authorship compatibility. Defaults to
	// serviceParticipant().
	owner protocol.ParticipantInfo

	// apiKey gates the JSON-RPC endpoint (A9). When non-empty, every /a2a
	// request must carry it in the apiKeyHeader header or gets 401, and the
	// card advertises the apiKey security scheme. This is the A2A-facing gate;
	// the bridge's OWN auth to the hugen API is the client token. Set from
	// HUGEN_A2A_API_KEY.
	apiKey string

	// allowOpen lets the bridge serve an unauthenticated /a2a endpoint. Without
	// it, Run fails closed when no API key is set. Set via HUGEN_A2A_ALLOW_OPEN=1
	// for a throwaway local run; never over a tunnel.
	allowOpen bool

	// client is the native HTTP API client the bridge drives hugen through.
	client *hugenclient.Client
	// reg maps contextIds to durable root sessions. Built in Run; the executor
	// resolves through it and Cancel forgets through it.
	reg *contextRegistry
}

// Option configures the Server.
type Option func(*Server)

func WithLogger(l *slog.Logger) Option { return func(a *Server) { a.logger = l } }

// WithBaseURL sets the public URL the agent card advertises (the value a
// client dials). In a tunnel deployment this is the tunnel hostname.
func WithBaseURL(u string) Option { return func(a *Server) { a.baseURL = u } }

// WithListenPort sets the dedicated listener port.
func WithListenPort(p int) Option { return func(a *Server) { a.listenPort = p } }

// WithAgentIdentity overrides the card's name/description.
func WithAgentIdentity(name, desc string) Option {
	return func(a *Server) {
		if name != "" {
			a.agentName = name
		}
		if desc != "" {
			a.agentDesc = desc
		}
	}
}

// WithServiceIdentity overrides the service identity label. Empty fields keep
// the default (serviceParticipant()).
func WithServiceIdentity(id, name string) Option {
	return func(a *Server) {
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
func WithAPIKey(key string) Option { return func(a *Server) { a.apiKey = strings.TrimSpace(key) } }

// WithAllowOpen permits serving an unauthenticated /a2a endpoint. Without it,
// Run fails closed when no API key is set.
func WithAllowOpen(v bool) Option { return func(a *Server) { a.allowOpen = v } }

// New constructs an A2A bridge server driving hugen through client.
func New(client *hugenclient.Client, opts ...Option) *Server {
	a := &Server{
		logger:    slog.Default(),
		agentName: defaultAgentName,
		agentDesc: defaultAgentDesc,
		owner:     serviceParticipant(),
		client:    client,
	}
	for _, o := range opts {
		o(a)
	}
	return a
}

// Run builds the a2asrv handler stack, mounts the agent card (+ legacy alias)
// and the JSON-RPC transport on a dedicated http.Server, and serves until ctx
// cancels.
func (a *Server) Run(ctx context.Context) error {
	if a.logger == nil {
		a.logger = slog.Default()
	}
	if a.client == nil {
		return fmt.Errorf("a2a: nil hugen client")
	}
	// The registry opens/resumes durable roots via the HTTP API on the bridge
	// lifecycle ctx — sessions live server-side (the API owns their Run loop),
	// so no per-request-ctx lifetime concern here.
	a.reg = newContextRegistry(clientRootStore{client: a.client, ctx: ctx}, a.logger)

	// A10: artifact delivery. Published files surface as A2A FileParts pointing
	// at a signed download URL this bridge serves — PROXYING the bytes from the
	// hugen API (the bridge has no local artifact store; H8). The signing secret
	// is the API key when set (ties artifact access to the same trust) else a
	// fresh random one.
	var artifactURL func(rootID, id string) string
	var artifactFetch artifactResolver
	artifactSecret := a.apiKey
	if artifactSecret == "" {
		// L1: never fall back to a known constant secret (which would make every
		// signed URL forgeable). If crypto/rand is unavailable (effectively
		// never), artifact delivery stays disabled.
		if s, err := randomArtifactSecret(); err != nil {
			a.logger.Warn("a2a: artifact delivery DISABLED — no signing secret", "err", err)
		} else {
			artifactSecret = s
		}
	}
	if artifactSecret != "" {
		base := a.baseURL
		artifactURL = func(rootID, id string) string {
			return signedArtifactURL(base, artifactSecret, rootID, id, time.Now())
		}
		artifactFetch = func(ctx context.Context, root, id string) (io.ReadCloser, error) {
			return a.client.DownloadArtifact(ctx, root, id)
		}
		a.logger.Info("a2a: by-ref artifact delivery enabled (proxied)", "endpoint", artifactPathPrefix)
	}

	card := a.buildCard()
	handler := a2asrv.NewHandler(newSessionExecutor(a.logger, a.reg, clientFrameIO{client: a.client}, a.owner, artifactURL))
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
		if artifactFetch != nil {
			mux.Handle(artifactPathPrefix, artifactDownloadHandler(artifactSecret, artifactFetch, a.logger))
		}
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
func (a *Server) apiKeyGate(next http.Handler) http.Handler {
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
func (a *Server) buildCard() *a2a.AgentCard {
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
