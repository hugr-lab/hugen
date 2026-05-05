package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/hugr-lab/hugen/pkg/config"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// New builds a Provider from a runtime-side tool.Spec — the entry
// point providers.Builder dispatches to. The auth.Service handles
// spec.Auth (HTTP RoundTripper or stdio bootstrap mint);
// workspaceRoot pins the WORKSPACES_ROOT env stdio children land
// under. log captures connection-level events; pass
// slog.New(slog.DiscardHandler) for a silent build.
//
// New does NOT register the provider with any ToolManager — the
// caller decides where it lives (root or per-session child).
//
// Tests that build the wire-level Spec directly use NewWithSpec.
func New(ctx context.Context, spec tool.Spec, authSvc *auth.Service, workspaceRoot string, log *slog.Logger) (*Provider, error) {
	cfgSpec := toConfigSpec(spec)
	wireSpec, cleanups, err := buildSpec(cfgSpec, authSvc, workspaceRoot)
	if err != nil {
		return nil, err
	}
	if spec.Cwd != "" {
		wireSpec.Cwd = spec.Cwd
	}
	prov, err := NewWithSpec(ctx, wireSpec, log)
	if err != nil {
		runCleanups(cleanups)
		return nil, err
	}
	prov.SetOnClose(cleanups)
	return prov, nil
}

// buildSpec turns a config.ToolProviderSpec (operator-authored
// YAML) into the wire-level Spec consumed by NewWithSpec. The
// `auth:` field on the spec drives credential injection through
// one mechanism and two transports:
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
// workspaceRoot is the runtime-managed parent directory every
// stdio child must write under — when non-empty, the spec's env
// gets a WORKSPACES_ROOT entry pointing at it, overriding any
// operator-supplied value. Empty string disables injection
// (tests; deployments without session.Workspace).
func buildSpec(spec config.ToolProviderSpec, authSvc *auth.Service, workspaceRoot string) (Spec, []func(), error) {
	out := Spec{
		Name:       spec.Name,
		Lifetime:   parseLifetime(spec.Lifetime, isHTTPTransport(spec.Transport)),
		PermObject: "hugen:tool:" + spec.Name,
	}

	if isHTTPTransport(spec.Transport) {
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

// toConfigSpec projects a runtime-side tool.Spec back into the
// operator-shaped config.ToolProviderSpec — the input shape
// buildSpec consumes. Pure field-by-field copy; pinned by
// TestToConfigSpec_FieldByField so future refactors can't silently
// drop a field.
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

// isHTTPTransport reports whether the transport label denotes an
// HTTP-family wire protocol. Mirrors tool.IsHTTPTransport but
// kept package-private here so the projection stays self-contained.
func isHTTPTransport(t string) bool {
	switch strings.ToLower(t) {
	case "http", "streamable-http", "sse":
		return true
	default:
		return false
	}
}

// parseLifetime applies the default rule: HTTP/SSE → per_agent
// (one connection for the whole process); stdio → per_session
// (the bash-mcp pattern). Explicit cfg.Lifetime always wins.
func parseLifetime(s string, httpDefault bool) tool.Lifetime {
	switch strings.ToLower(s) {
	case "per_session":
		return tool.LifetimePerSession
	case "per_agent":
		return tool.LifetimePerAgent
	}
	if httpDefault {
		return tool.LifetimePerAgent
	}
	return tool.LifetimePerSession
}

func runCleanups(fns []func()) {
	for _, fn := range fns {
		if fn != nil {
			fn()
		}
	}
}
