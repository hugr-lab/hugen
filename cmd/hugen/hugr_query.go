package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"

	httpapi "github.com/hugr-lab/hugen/pkg/adapter/http"
	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/hugr-lab/hugen/pkg/config"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// hugrQueryBuilder implements tool.ProviderBuilder for the
// `type: hugr-query` config kind. It spawns the in-tree
// `mcp/hugr-query` binary as a stdio MCP server and supplies the
// runtime-only env (bootstrap token, agent-token URL, agent id,
// workspaces root, shared dir) that operators can't write into
// config.yaml because those values are minted at boot.
//
// The cleanup callback returned by Build revokes the spawn entry
// from AgentTokenStore — without it, a stale spawn's previously
// issued tokens would keep authenticating against the endpoint
// after the MCP exits.
type hugrQueryBuilder struct {
	authStore *httpapi.AgentTokenStore
	baseURL   string // agent's loopback HTTP base URL — TokenURL anchor
	hugrURL   string // upstream Hugr GraphQL endpoint
	stateDir  string // ${HUGEN_STATE} — workspaces parent
	sharedDir string // ${HUGEN_SHARED_ROOT}
	agentID   string
	log       *slog.Logger
}

func (b *hugrQueryBuilder) Build(ctx context.Context, spec config.ToolProviderSpec) (tool.ToolProvider, []func(), error) {
	if spec.Command == "" {
		return nil, nil, fmt.Errorf("hugr-query: command is empty (set tool_providers[].command, e.g. ./bin/hugr-query)")
	}
	if b.hugrURL == "" {
		return nil, nil, fmt.Errorf("hugr-query: HUGR_URL not configured (Hugr is optional — drop the provider entry to deploy without Hugr)")
	}

	bootstrap, err := mintBootstrapToken()
	if err != nil {
		return nil, nil, fmt.Errorf("mint bootstrap: %w", err)
	}
	revoke, err := b.authStore.RegisterSpawn(bootstrap)
	if err != nil {
		return nil, nil, fmt.Errorf("register spawn: %w", err)
	}

	// Compose env: operator-supplied keys (cfg.Env) layered under
	// runtime-controlled keys. Runtime wins so a misconfigured
	// HUGR_URL/HUGR_TOKEN_URL in YAML cannot escape the bootstrap
	// flow.
	env := make(map[string]string, len(spec.Env)+8)
	for k, v := range spec.Env {
		env[k] = v
	}
	env["HUGR_URL"] = b.hugrURL
	env["HUGR_TOKEN_URL"] = b.baseURL + "/api/auth/agent-token"
	env["HUGR_ACCESS_TOKEN"] = bootstrap
	env["WORKSPACES_ROOT"] = filepath.Join(b.stateDir, "workspaces")
	if b.sharedDir != "" {
		env["SHARED_DIR"] = b.sharedDir
	}
	env["HUGEN_AGENT_ID"] = b.agentID
	// Optional ceiling/default — leave to operator if they want
	// to override; defaults baked into the binary cover the rest.
	if _, has := env["HUGR_QUERY_TIMEOUT_MS"]; !has {
		env["HUGR_QUERY_TIMEOUT_MS"] = "3600000"
	}
	if _, has := env["HUGR_QUERY_MAX_TIMEOUT_MS"]; !has {
		env["HUGR_QUERY_MAX_TIMEOUT_MS"] = "86400000"
	}

	mcpSpec := tool.MCPProviderSpec{
		Name:        spec.Name,
		Command:     spec.Command,
		Args:        spec.Args,
		Env:         env,
		Lifetime:    tool.LifetimePerAgent,
		PermObject:  "hugen:tool:" + spec.Name,
		Description: "Hugr GraphQL query → file output (Parquet/JSON) under per-session data dir",
		Transport:   tool.TransportStdio,
	}
	prov, err := tool.NewMCPProvider(ctx, mcpSpec, b.log)
	if err != nil {
		revoke()
		return nil, nil, fmt.Errorf("spawn hugr-query: %w", err)
	}
	cleanup := []func(){revoke}
	return prov, cleanup, nil
}

// mintBootstrapToken returns 32 bytes of hex-encoded random — long
// enough that an attacker can't guess it inside the 30 s
// bootstrap window.
func mintBootstrapToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// agentTokenSourceFromAuth adapts auth.Service into the
// httpapi.AgentTokenSource interface. Returns the agent's current
// Hugr access token plus its remaining lifetime so the consumer
// (hugr-query's RemoteStore) refreshes on the JWT's actual cadence
// rather than a hardcoded ceiling.
//
// When the underlying source exposes TokenWithTTL (oidc.Source,
// hugr.RemoteStore — see those packages), we use it. Otherwise we
// fall back to plain Token + a conservative TTL=0 sentinel; the
// AgentTokenStore floors zero to 60 s.
type agentTokenSourceFromAuth struct {
	svc *auth.Service
}

type ttlAware interface {
	TokenWithTTL(ctx context.Context) (string, int, error)
}

func (a *agentTokenSourceFromAuth) Token(ctx context.Context) (string, int, error) {
	ts, ok := a.svc.TokenStore("hugr")
	if !ok {
		return "", 0, fmt.Errorf("auth source %q not registered", "hugr")
	}
	if ttl, ok := ts.(ttlAware); ok {
		return ttl.TokenWithTTL(ctx)
	}
	tok, err := ts.Token(ctx)
	return tok, 0, err
}

// buildAgentTokenStore returns the loopback /api/auth/agent-token
// store backed by the agent's current Hugr token. When no `hugr`
// auth source is registered (US5: no-Hugr deployment), returns
// (nil, nil) so the caller mounts no handler.
func buildAgentTokenStore(authSvc *auth.Service) (*httpapi.AgentTokenStore, error) {
	if _, ok := authSvc.TokenStore("hugr"); !ok {
		return nil, nil
	}
	return httpapi.NewAgentTokenStore(&agentTokenSourceFromAuth{svc: authSvc}, httpapi.AgentTokenOptions{})
}

// mountAgentTokenHandler binds the AgentTokenStore handler at
// /api/auth/agent-token. Replaces the 501 stub from T040.
func mountAgentTokenHandler(mux *http.ServeMux, store *httpapi.AgentTokenStore) {
	if store == nil {
		// No hugr source → no consumer. Leave the path unmounted
		// so a probe sees 404 (clean signal) rather than a 501.
		return
	}
	mux.Handle("/api/auth/agent-token", http.HandlerFunc(store.Handle))
}
