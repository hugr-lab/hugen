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

// ProviderBuilder turns a config.ToolProviderSpec into a live
// ToolProvider. Different `type` values get different builders.
//
// Built-in: `type: mcp` is handled by the default MCP builder
// (see Init). Operators register additional builders at boot for
// runtime-managed kinds — `hugr-query` mints a per-spawn agent
// token, `python-sandbox` (later) attaches a per-session venv,
// etc. Each builder owns its own knowledge of listener URL,
// secrets, paths — pkg/tool does not need to know.
//
// The cleanups slice is run on RemoveProvider/Close. Use it to
// revoke runtime-minted secrets, free temp dirs, etc.
type ProviderBuilder interface {
	Build(ctx context.Context, spec config.ToolProviderSpec) (provider ToolProvider, cleanups []func(), err error)
}

// ProviderBuilderFunc adapts a plain function into a
// ProviderBuilder. Convenient for in-line registration.
type ProviderBuilderFunc func(ctx context.Context, spec config.ToolProviderSpec) (ToolProvider, []func(), error)

// Build implements ProviderBuilder.
func (f ProviderBuilderFunc) Build(ctx context.Context, spec config.ToolProviderSpec) (ToolProvider, []func(), error) {
	return f(ctx, spec)
}

// AuthResolverFunc adapts an ordinary function into an
// AuthResolver. Useful for tests and small wirings.
type AuthResolverFunc func(name string) (http.RoundTripper, error)

// RoundTripper implements AuthResolver.
func (f AuthResolverFunc) RoundTripper(name string) (http.RoundTripper, error) { return f(name) }

// Init opens the per_agent MCP entries from the configuration
// passed to NewToolManager and registers them as global providers.
// Per_session entries are skipped — they are spawned in the
// session.Resources.Acquire path (bash-mcp pattern).
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
		if EffectiveLifetime(spec) != LifetimePerAgent {
			continue
		}
		builder := m.builderFor(spec.Type)
		if builder == nil {
			m.log.Warn("provider disabled: unknown type",
				"provider", spec.Name, "type", spec.Type)
			continue
		}
		connectCtx, cancel := context.WithTimeout(ctx, m.connectTimeout)
		prov, cleanups, err := builder.Build(connectCtx, spec)
		cancel()
		if err != nil {
			m.log.Warn("provider disabled: build failed",
				"provider", spec.Name, "type", spec.Type, "err", err)
			runCleanups(cleanups)
			continue
		}
		if err := m.AddProvider(prov); err != nil {
			_ = prov.Close()
			runCleanups(cleanups)
			m.log.Warn("provider disabled: register failed",
				"provider", spec.Name, "err", err)
			continue
		}
		m.recordCleanups(spec.Name, cleanups)
		m.log.Info("provider ready",
			"provider", spec.Name, "type", spec.Type)
	}
	return nil
}

// builderFor returns the ProviderBuilder registered for typeName.
// The empty type and `mcp` both map to the built-in MCP builder.
// nil indicates no builder registered.
func (m *ToolManager) builderFor(typeName string) ProviderBuilder {
	t := strings.ToLower(typeName)
	if t == "" {
		t = "mcp"
	}
	if b, ok := m.builders[t]; ok {
		return b
	}
	if t == "mcp" {
		return defaultMCPBuilder{m: m}
	}
	return nil
}

// defaultMCPBuilder is the built-in handler for `type: mcp`. It
// reuses BuildMCPProviderSpec + NewMCPProvider — same path as
// before the builder abstraction landed.
type defaultMCPBuilder struct{ m *ToolManager }

func (b defaultMCPBuilder) Build(ctx context.Context, spec config.ToolProviderSpec) (ToolProvider, []func(), error) {
	mcpSpec, err := BuildMCPProviderSpec(spec, b.m.authResolver)
	if err != nil {
		return nil, nil, err
	}
	prov, err := NewMCPProvider(ctx, mcpSpec, b.m.log)
	if err != nil {
		return nil, nil, err
	}
	return prov, nil, nil
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

// EffectiveLifetime applies the default rule: HTTP/SSE → per_agent
// (one connection for the whole process); stdio → per_session
// (the bash-mcp pattern). Explicit cfg.Lifetime always wins.
//
// Exported for the session package, which uses it to decide which
// providers to spawn on session.Open vs at boot.
func EffectiveLifetime(spec config.ToolProviderSpec) Lifetime {
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
