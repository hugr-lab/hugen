package tool

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/hugr-lab/hugen/pkg/config"
)

// AuthResolver hands back an http.RoundTripper for a named auth
// source. Implementations typically look up a TokenStore in
// pkg/auth.Service and wrap it via auth.Transport. pkg/tool stays
// agnostic of the concrete auth machinery — the resolver is the
// only seam between transports and the auth registry.
type AuthResolver interface {
	RoundTripper(name string) (http.RoundTripper, error)
}

// AuthResolverFunc adapts an ordinary function into an
// AuthResolver. Useful for tests and small wirings.
type AuthResolverFunc func(name string) (http.RoundTripper, error)

// RoundTripper implements AuthResolver.
func (f AuthResolverFunc) RoundTripper(name string) (http.RoundTripper, error) { return f(name) }

// Init opens the per_agent MCP entries from the configuration
// passed to NewToolManager and registers them as global providers.
// Per_session entries are skipped — they are spawned in the
// SessionLifecycle.OnOpen hook (bash-mcp pattern).
//
// A per-provider failure (bad config, unreachable endpoint,
// initialise error) is logged and the provider is skipped — Init
// never aborts boot for a single misconfigured / down provider.
// Tools from skipped providers stay unavailable until the operator
// fixes config and restarts (periodic reconnect is a future hook).
// Init returns an error only for caller-supplied conditions
// (e.g. ctx cancelled).
func (m *ToolManager) Init(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if m.providersView == nil {
		return nil
	}
	m.providersView.OnUpdate(func() {
		m.log.Warn("tool: live reload of tool_providers not implemented; restart hugen to apply changes")
	})
	for _, spec := range m.providersView.Providers() {
		if !strings.EqualFold(spec.Type, "mcp") {
			continue
		}
		if effectiveLifetime(spec) != LifetimePerAgent {
			continue
		}

		mcpSpec, err := BuildMCPProviderSpec(spec, m.authResolver)
		if err != nil {
			m.log.Warn("mcp provider disabled: bad config",
				"provider", spec.Name, "err", err)
			continue
		}
		connectCtx, cancel := context.WithTimeout(ctx, m.connectTimeout)
		prov, err := NewMCPProvider(connectCtx, mcpSpec, m.log)
		cancel()
		if err != nil {
			m.log.Warn("mcp provider disabled: connect failed",
				"provider", spec.Name,
				"endpoint", mcpSpec.Endpoint,
				"err", err)
			continue
		}
		if err := m.AddProvider(prov); err != nil {
			_ = prov.Close()
			m.log.Warn("mcp provider disabled: register failed",
				"provider", spec.Name, "err", err)
			continue
		}
		m.log.Info("mcp provider ready",
			"provider", spec.Name,
			"transport", string(mcpSpec.Transport),
			"endpoint", mcpSpec.Endpoint)
	}
	return nil
}

// BuildMCPProviderSpec turns a config.ToolProviderSpec into the
// pkg/tool.MCPProviderSpec the runtime constructs. For HTTP/SSE
// transports it consults the AuthResolver to obtain a
// bearer-injecting RoundTripper from spec.Auth.
//
// Exported so admin paths (`mcp_add_server`) and tests can build a
// single spec without going through the full Init loop.
func BuildMCPProviderSpec(spec config.ToolProviderSpec, resolver AuthResolver) (MCPProviderSpec, error) {
	out := MCPProviderSpec{
		Name:       spec.Name,
		Lifetime:   parseLifetime(spec.Lifetime, IsHTTPTransport(spec.Transport)),
		PermObject: "hugen:tool:" + spec.Name,
	}

	if IsHTTPTransport(spec.Transport) {
		if spec.Endpoint == "" {
			return out, fmt.Errorf("missing endpoint for transport %q", spec.Transport)
		}
		switch strings.ToLower(spec.Transport) {
		case "http", "streamable-http":
			out.Transport = TransportStreamableHTTP
		case "sse":
			out.Transport = TransportSSE
		}
		out.Endpoint = spec.Endpoint
		out.Headers = spec.Headers
		if spec.Auth != "" {
			if resolver == nil {
				return out, fmt.Errorf("auth %q requested but no resolver supplied", spec.Auth)
			}
			rt, err := resolver.RoundTripper(spec.Auth)
			if err != nil {
				return out, fmt.Errorf("auth %q: %w", spec.Auth, err)
			}
			out.RoundTripper = rt
		}
		return out, nil
	}

	// stdio per_agent (e.g. hugr-query in US2): inherits the
	// existing spawn shape used by bash-mcp.
	out.Transport = TransportStdio
	out.Command = spec.Command
	out.Args = spec.Args
	out.Env = spec.Env
	if spec.Command == "" {
		return out, fmt.Errorf("stdio provider missing command")
	}
	return out, nil
}

// IsHTTPTransport reports whether the transport label denotes an
// HTTP-family wire protocol (streamable-http or SSE).
func IsHTTPTransport(t string) bool {
	switch strings.ToLower(t) {
	case "http", "streamable-http", "sse":
		return true
	default:
		return false
	}
}

// effectiveLifetime applies the default rule: HTTP/SSE → per_agent
// (one connection for the whole process); stdio → per_session
// (the bash-mcp pattern). Explicit cfg.Lifetime always wins.
func effectiveLifetime(spec config.ToolProviderSpec) Lifetime {
	return parseLifetime(spec.Lifetime, IsHTTPTransport(spec.Transport))
}

func parseLifetime(s string, httpDefault bool) Lifetime {
	switch strings.ToLower(s) {
	case "per_session":
		return LifetimePerSession
	case "per_agent":
		return LifetimePerAgent
	}
	if httpDefault {
		return LifetimePerAgent
	}
	return LifetimePerSession
}
