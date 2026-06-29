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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
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

	// host is the runtime side of the adapter contract, captured in Run.
	host adapter.Host
	// reg maps contextIds to durable root sessions (A2). Built in Run;
	// the executor resolves through it, and idle-GC (A8) forgets through it.
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
	a.reg = newContextRegistry(hostRootStore{host: host, owner: a.owner}, a.logger)

	card := a.buildCard()
	handler := a2asrv.NewHandler(newEchoExecutor(a.logger, a.reg))
	jsonrpc := a2asrv.NewJSONRPCHandler(handler)
	cardHandler := a2asrv.NewStaticAgentCardHandler(card)

	register := func(mux *http.ServeMux) {
		mux.Handle(a2asrv.WellKnownAgentCardPath, cardHandler)
		mux.Handle(legacyCardPath, cardHandler)
		mux.Handle(jsonRPCPath, jsonrpc)
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

// buildCard assembles the v1.0 agent card. Stage 1 advertises one skill
// ("hugr analyst") over the JSON-RPC interface; missions/tasks map to
// additional AgentSkills later (spec §1 non-goals).
func (a *Adapter) buildCard() *a2a.AgentCard {
	iface := a2a.NewAgentInterface(a.baseURL+jsonRPCPath, a2a.TransportProtocolJSONRPC)
	iface.ProtocolVersion = a2a.Version // "1.0"
	return &a2a.AgentCard{
		Name:                a.agentName,
		Description:         a.agentDesc,
		Version:             agentVersion,
		SupportedInterfaces: []*a2a.AgentInterface{iface},
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
}
