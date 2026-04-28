package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"github.com/hugr-lab/hugen/pkg/auth/sources"
	"github.com/hugr-lab/hugen/pkg/auth/sources/oidc"
)

// MCPSource is a Source that will authenticate against a specific
// MCP server via OAuth. Scope of commit #2 is the shape + OwnsState
// plumbing so SourceRegistry can route callbacks to it. The actual
// OAuth handshake wraps an OIDCStore that is lazily initialised
// from the MCP server's 401-challenge resource metadata — that
// end-to-end flow lands in commit #3.
//
// MCP SDK integration: the SDK's auth.OAuthHandler interface uses
// an unexported sealing method (isOAuthHandler), so MCPSource
// itself cannot satisfy it directly. In commit #3 we will either
// use the SDK's AuthorizationCodeHandler as the inner engine and
// expose its instance via an Adapter, or convince upstream to
// widen the interface. For now MCPSource stands on its own as a
// Source, and no MCP transport is wired to use it yet.
type MCPSource struct {
	name   string
	logger *slog.Logger

	mu    sync.RWMutex
	inner *oidc.Source // lazily initialised on first Authorize
}

// NewMCPSource builds a skeleton MCPSource. Name is required.
func NewMCPSource(name string, logger *slog.Logger) (*MCPSource, error) {
	if name == "" {
		return nil, fmt.Errorf("mcp source: Name is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &MCPSource{name: name, logger: logger}, nil
}

// Name implements Source.
func (s *MCPSource) Name() string { return s.name }

// Token returns the underlying OIDCStore token, or an error when
// the OAuth handshake has not yet run.
func (s *MCPSource) Token(ctx context.Context) (string, error) {
	s.mu.RLock()
	inner := s.inner
	s.mu.RUnlock()
	if inner == nil {
		return "", fmt.Errorf("mcp source %q: not authorized yet", s.name)
	}
	return inner.Token(ctx)
}

// Login proxies to the underlying OIDCStore once it exists.
func (s *MCPSource) Login(ctx context.Context) error {
	s.mu.RLock()
	inner := s.inner
	s.mu.RUnlock()
	if inner == nil {
		return fmt.Errorf("mcp source %q: not authorized yet", s.name)
	}
	return inner.Login(ctx)
}

// OwnsState implements Source using the "<name>." prefix convention.
func (s *MCPSource) OwnsState(state string) bool {
	return sources.StateOwnedBy(s.name, state)
}

// HandleCallback delegates to the inner OIDCStore after lazy
// discovery.
func (s *MCPSource) HandleCallback(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	inner := s.inner
	s.mu.RUnlock()
	if inner == nil {
		http.Error(w, "mcp source not initialized", http.StatusBadRequest)
		return
	}
	inner.HandleCallback(w, r)
}

// setInner is exposed for future commit #3 wiring — once the
// resource metadata is parsed, the constructor builds the OIDCStore
// and installs it here.
func (s *MCPSource) setInner(inner *oidc.Source) {
	s.mu.Lock()
	s.inner = inner
	s.mu.Unlock()
}
