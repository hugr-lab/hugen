// Package a2a builds HTTP handlers for the A2A (agent-to-agent) JSON-RPC
// transport: the agent card under /.well-known/agent.json and the
// /invoke endpoint wired through ADK's adka2a executor.
//
// The runtime owns listener lifecycle and orchestration; this package
// stays a pure helper library so it can be mounted on either an
// http.ServeMux (A2A mode) or any http.Handler tree (devui mode).
package a2a

import (
	"net/http"

	"github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2asrv"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/server/adka2a"
	adksession "google.golang.org/adk/session"
)

// BuildHandlers returns the two HTTP handlers that make up the A2A
// surface: the static agent card and the JSON-RPC invoke endpoint.
//
// baseURL is the externally-visible URL prefix the agent card
// advertises for /invoke (e.g. "http://localhost:10000"). It must be
// reachable from A2A clients — in devui mode this is the *A2A*
// listener base URL, not the DevUI listener.
func BuildHandlers(
	a agent.Agent,
	sessionSvc adksession.Service,
	baseURL string,
) (card, invoke http.Handler) {
	agentCard := &a2a.AgentCard{
		Name:               a.Name(),
		Description:        a.Description(),
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		URL:                baseURL + "/invoke",
		PreferredTransport: a2a.TransportProtocolJSONRPC,
		Skills:             adka2a.BuildAgentSkills(a),
		Capabilities:       a2a.AgentCapabilities{Streaming: true},
	}
	executor := adka2a.NewExecutor(adka2a.ExecutorConfig{
		RunnerConfig: runner.Config{
			AppName:        a.Name(),
			Agent:          a,
			SessionService: sessionSvc,
		},
	})
	return a2asrv.NewStaticAgentCardHandler(agentCard),
		a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(executor))
}
