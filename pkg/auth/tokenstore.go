// Package auth owns token management + OAuth flow dispatch for the
// hugr-agent.
//
// Types in two layers:
//   - Source (see source.go) is the full stateful token provider:
//     Token() for bearer issuance, Login() + HandleCallback() for
//     browser-based OAuth, OwnsState() so the shared /auth/callback
//     dispatcher can route back to the right Source.
//   - TokenStore (this file) is the thin slice every caller that
//     just needs a bearer reaches through: auth.Transport(store,
//     base) wraps an http.RoundTripper with Authorization: Bearer …
//
// Concrete Sources shipped today: RemoteStore (pre-seeded token +
// external refresh URL), OIDCStore (PKCE + /.well-known discovery),
// MCPSource (skeleton for per-MCP OAuth). Selection of the hugr
// Source at boot lives in cmd/agent — see auth.BuildHugrSource.
package auth

import "context"

// TokenStore provides access tokens for Hugr API authentication.
type TokenStore interface {
	// Token returns a valid access token. Implementations must handle
	// refresh/exchange internally — callers always get a ready-to-use token.
	Token(ctx context.Context) (string, error)
}
