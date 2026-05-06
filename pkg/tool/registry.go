package tool

import (
	"context"
	"strings"
	"time"

	"github.com/hugr-lab/hugen/pkg/config"
)

// Init loads the per_agent providers from configuration via the
// Spec-driven AddBySpec dispatch when a view + builder are wired.
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
//
// Phase 4.1c step 34 retired the background Reconnector loop;
// recovery is lazy via pkg/tool/providers/recovery.Wrap, applied
// when the provider is built (providers.Builder /
// pkg/session/lifecycle).
func (m *ToolManager) Init(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
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
