package mcp

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/hugr-lab/hugen/pkg/config"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// Provider is the MCP ToolProvider as exposed by the new
// pkg/tool.Spec-driven API. During phase 4.1a stage A it wraps
// the existing *tool.MCPProvider; the actual MCP wire logic is
// relocated into this subpackage in a later stage. The
// observable contract — Name, Lifetime, List, Call, Subscribe,
// Close — matches tool.ToolProvider.
//
// Lifecycle:
//   - New constructs the provider, opens the MCP transport, and
//     records any teardown callbacks the construction returned
//     (today: stdio-auth revoke for spec.Auth-bound providers).
//   - Close drops the underlying client and runs every onClose
//     callback. Idempotent.
//
// onClose ownership replaces the phase-3 ToolManager.cleanups
// map: each provider stores its own teardown so the manager
// surface stays free of side state. Future steps remove the
// manager-level map; for now both paths coexist.
type Provider struct {
	inner   *tool.MCPProvider
	onClose []func()
}

// New constructs a Provider from a runtime-side tool.Spec. The
// auth.Service handles spec.Auth (HTTP RoundTripper or stdio
// bootstrap mint); workspaceRoot pins the WORKSPACES_ROOT env
// stdio children land under. log captures connection-level
// events; pass slog.New(slog.DiscardHandler) for a silent build.
//
// New does NOT register the provider with any ToolManager — the
// caller decides where it lives (root or per-session child).
func New(ctx context.Context, spec tool.Spec, authSvc *auth.Service, workspaceRoot string, log *slog.Logger) (*Provider, error) {
	cfgSpec := toConfigSpec(spec)
	legacySpec, cleanups, err := tool.BuildMCPProviderSpec(cfgSpec, authSvc, workspaceRoot)
	if err != nil {
		return nil, err
	}
	if spec.Cwd != "" {
		legacySpec.Cwd = spec.Cwd
	}
	inner, err := tool.NewMCPProvider(ctx, legacySpec, log)
	if err != nil {
		runCleanups(cleanups)
		return nil, err
	}
	return &Provider{inner: inner, onClose: cleanups}, nil
}

// Name reports the provider short name (matches the prefix of
// every Tool.Name it exposes).
func (p *Provider) Name() string { return p.inner.Name() }

// Lifetime tags the provider as per_agent / per_session / external.
func (p *Provider) Lifetime() tool.Lifetime { return p.inner.Lifetime() }

// List returns the provider's current tool catalogue.
func (p *Provider) List(ctx context.Context) ([]tool.Tool, error) { return p.inner.List(ctx) }

// Call dispatches a single tool call; args MUST be the substituted
// payload returned by tool.ToolManager.Resolve.
func (p *Provider) Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	return p.inner.Call(ctx, name, args)
}

// Subscribe streams provider events (tool-list changes, health,
// terminations). Returns nil when the underlying MCP client has
// no event stream.
func (p *Provider) Subscribe(ctx context.Context) (<-chan tool.ProviderEvent, error) {
	return p.inner.Subscribe(ctx)
}

// Close drops the underlying MCP client and runs every onClose
// callback (revoke minted stdio-auth, etc.). Idempotent.
func (p *Provider) Close() error {
	closeErr := p.inner.Close()
	runCleanups(p.onClose)
	p.onClose = nil
	return closeErr
}

// Inner returns the wrapped *tool.MCPProvider so external code
// can wire integration that still depends on the legacy type
// (today: ToolManager.AddProvider stale-hook setup). The shortcut
// retires once the implementation lives natively in this
// subpackage and the public surface no longer references the
// legacy provider.
func (p *Provider) Inner() *tool.MCPProvider { return p.inner }

func toConfigSpec(spec tool.Spec) config.ToolProviderSpec {
	return config.ToolProviderSpec{
		Name:      spec.Name,
		Type:      spec.Type,
		Transport: spec.Transport,
		Lifetime:  spec.Lifetime.String(),
		Command:   spec.Command,
		Args:      spec.Args,
		Env:       spec.Env,
		Endpoint:  spec.Endpoint,
		Headers:   spec.Headers,
		Auth:      spec.Auth,
	}
}

func runCleanups(fns []func()) {
	for _, fn := range fns {
		if fn != nil {
			fn()
		}
	}
}
