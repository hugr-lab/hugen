package tool

import (
	"context"
	"fmt"
	"strings"

	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/hugr-lab/hugen/pkg/config"
)

// legacyProviderBuilder is the phase-3-era construction interface.
// It turns a config.ToolProviderSpec into a live ToolProvider plus
// a slice of teardown callbacks. Phase 4.1a replaces it with the
// runtime-side ProviderBuilder (see builder.go) that consumes a
// type-agnostic tool.Spec; this interface stays internal until the
// drop in stage A.7.
//
// Operators register additional builders at boot for non-MCP
// runtime-managed kinds. Each builder owns its own knowledge of
// listener URL, secrets, paths — pkg/tool does not need to know.
//
// The cleanups slice is run on RemoveProvider/Close. Use it to
// revoke runtime-minted secrets, free temp dirs, etc.
type legacyProviderBuilder interface {
	Build(ctx context.Context, spec config.ToolProviderSpec) (provider ToolProvider, cleanups []func(), err error)
}

// legacyProviderBuilderFunc adapts a plain function into a
// legacyProviderBuilder. Convenient for in-line registration.
type legacyProviderBuilderFunc func(ctx context.Context, spec config.ToolProviderSpec) (ToolProvider, []func(), error)

// Build implements legacyProviderBuilder.
func (f legacyProviderBuilderFunc) Build(ctx context.Context, spec config.ToolProviderSpec) (ToolProvider, []func(), error) {
	return f(ctx, spec)
}

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
	if m.reconnector != nil {
		// Use the caller's ctx as the cancel root so a process-shutdown
		// ctx cleanly exits the reconnector loop alongside everything
		// else. Close() also calls Stop() as belt-and-braces.
		m.reconnector.Start(ctx)
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

// builderFor returns the legacyProviderBuilder registered for
// typeName. The empty type and `mcp` both map to the built-in MCP
// builder. nil indicates no builder registered.
func (m *ToolManager) builderFor(typeName string) legacyProviderBuilder {
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
	mcpSpec, cleanups, err := BuildMCPProviderSpec(spec, b.m.auth, b.m.workspaceRoot)
	if err != nil {
		return nil, nil, err
	}
	prov, err := NewMCPProvider(ctx, mcpSpec, b.m.log)
	if err != nil {
		return nil, cleanups, err
	}
	return prov, cleanups, nil
}

// BuildMCPProviderSpec turns a config.ToolProviderSpec into the
// pkg/tool.MCPProviderSpec the runtime constructs. The `auth:`
// field on the spec drives credential injection through one
// mechanism and two transports:
//
//   - HTTP/SSE: the auth.Service issues a bearer-injecting
//     RoundTripper via auth.Transport(authSvc.TokenStore(name), ...).
//   - stdio: the auth.Service mints a per-spawn StdioAuth — a
//     loopback bootstrap token + token URL — and the env keys it
//     contributes (HUGR_TOKEN_URL, HUGR_ACCESS_TOKEN) are merged
//     into the spawn env. The returned cleanups slice carries the
//     RevokeFunc so RemoveProvider/Close drops the spawn from the
//     loopback store.
//
// workspaceRoot is the runtime-managed parent directory every stdio
// child must write under — when non-empty, the spec's env gets a
// WORKSPACES_ROOT entry pointing at it, overriding any operator-
// supplied value. Empty string disables injection (tests; deployments
// without session.Workspace).
//
// Exported so admin paths (`mcp_add_server`) and tests can build a
// single spec without going through the full Init loop.
func BuildMCPProviderSpec(spec config.ToolProviderSpec, authSvc *auth.Service, workspaceRoot string) (MCPProviderSpec, []func(), error) {
	out := MCPProviderSpec{
		Name:       spec.Name,
		Lifetime:   parseLifetime(spec.Lifetime, IsHTTPTransport(spec.Transport)),
		PermObject: "hugen:tool:" + spec.Name,
	}

	if IsHTTPTransport(spec.Transport) {
		if spec.Endpoint == "" {
			return out, nil, fmt.Errorf("missing endpoint for transport %q", spec.Transport)
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
			if authSvc == nil {
				return out, nil, fmt.Errorf("auth %q requested but no auth.Service supplied", spec.Auth)
			}
			ts, ok := authSvc.TokenStore(spec.Auth)
			if !ok {
				return out, nil, fmt.Errorf("auth source %q not registered", spec.Auth)
			}
			out.RoundTripper = auth.Transport(ts, nil)
		}
		return out, nil, nil
	}

	// stdio per_agent (e.g. hugr-query, python-mcp): inherits the
	// existing spawn shape used by bash-mcp. The runtime injects
	// WORKSPACES_ROOT here so per_agent children land in the same
	// on-disk tree as per_session bash-mcp; spec.Auth flips on the
	// loopback bootstrap injection.
	out.Transport = TransportStdio
	out.Command = spec.Command
	out.Args = spec.Args
	if spec.Command == "" {
		return out, nil, fmt.Errorf("stdio provider missing command")
	}
	merged := make(map[string]string, len(spec.Env)+3)
	for k, v := range spec.Env {
		merged[k] = v
	}
	if workspaceRoot != "" {
		merged["WORKSPACES_ROOT"] = workspaceRoot
	}

	if spec.Auth == "" {
		out.Env = merged
		return out, nil, nil
	}
	if authSvc == nil {
		return out, nil, fmt.Errorf("auth %q requested but no auth.Service supplied", spec.Auth)
	}
	sa, err := authSvc.NewStdioAuth(context.Background(), spec.Auth)
	if err != nil {
		return out, nil, fmt.Errorf("auth %q: %w", spec.Auth, err)
	}
	for k, v := range sa.Env() {
		merged[k] = v
	}
	out.Env = merged
	return out, []func(){sa.RevokeFunc}, nil
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
