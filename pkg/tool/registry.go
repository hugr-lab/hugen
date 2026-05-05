package tool

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/hugr-lab/hugen/pkg/config"
)

// Init starts the background reconnector loop and, when a view +
// builder are wired, loads the per_agent providers from
// configuration via the new Spec-driven AddBySpec dispatch.
// Per-session entries are skipped — they are spawned in the
// session.Resources.Acquire path (bash-mcp pattern).
//
// A per-provider failure (bad config, unreachable endpoint,
// initialise error) is logged and the provider is skipped — Init
// never aborts boot for a single misconfigured / down provider.
// Init returns an error only for caller-supplied conditions
// (e.g. ctx cancelled).
//
// Phase 4.1a stage A step 7d retired the legacy ProviderBuilder
// machinery — Init now dispatches exclusively through the
// providers.Builder injected via WithBuilder. The view's OnUpdate
// hook is wired with a placeholder warn log; live config reload
// of tool_providers lands in a future phase.
func (m *ToolManager) Init(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if m.parent == nil && m.reconnector != nil {
		// Use the caller's ctx as the cancel root so a process-shutdown
		// ctx cleanly exits the reconnector loop alongside everything
		// else. Close() also calls Stop() as belt-and-braces.
		m.reconnector.Start(ctx)
	}
	if m.view == nil {
		return nil
	}
	m.view.OnUpdate(func() {
		m.log.Warn("tool: live reload of tool_providers not implemented; restart hugen to apply changes")
	})
	return m.LoadConfig(ctx)
}

// LoadConfig iterates the wired view's per_agent entries and
// registers each via AddBySpec. Per-spec failures are logged and
// skipped — the loop continues so a single broken provider does
// not block others. nil view → no-op. The builder is consulted
// per-spec; specs that filter out (per_session) never reach
// AddBySpec, so a Manager configured with only per_session
// entries is OK without a wired Builder.
func (m *ToolManager) LoadConfig(ctx context.Context) error {
	if m.view == nil {
		return nil
	}
	for _, p := range m.view.Providers() {
		if EffectiveLifetime(p) != LifetimePerAgent {
			continue
		}
		if m.builder == nil {
			m.log.Warn("provider disabled: no ProviderBuilder configured",
				"provider", p.Name, "type", p.Type)
			continue
		}
		spec := SpecFromConfig(p)
		connectCtx, cancel := context.WithTimeout(ctx, defaultConnectTimeout)
		err := m.AddBySpec(connectCtx, spec)
		cancel()
		if err != nil {
			m.log.Warn("provider disabled",
				"provider", p.Name, "type", p.Type, "err", err)
			continue
		}
		m.log.Info("provider ready",
			"provider", p.Name, "type", p.Type)
	}
	return nil
}

// SpecFromConfig projects a config.ToolProviderSpec (operator-
// authored YAML) into the runtime-side tool.Spec consumed by
// ProviderBuilder. Field-by-field copy; Lifetime is resolved via
// EffectiveLifetime so the projection matches the pre-7d Init
// behaviour for stdio / HTTP defaults.
func SpecFromConfig(p config.ToolProviderSpec) Spec {
	return Spec{
		Name:      p.Name,
		Type:      p.Type,
		Transport: p.Transport,
		Lifetime:  EffectiveLifetime(p),
		Command:   p.Command,
		Args:      p.Args,
		Env:       p.Env,
		Endpoint:  p.Endpoint,
		Headers:   p.Headers,
		Auth:      p.Auth,
	}
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
// Exported so admin paths and tests can build a single spec
// without going through the full Init loop.
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

// defaultConnectTimeout caps a single per_agent provider Build
// call. Per-provider so a hung dial does not block sibling
// providers from loading.
const defaultConnectTimeout = 30 * time.Second
