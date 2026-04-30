package sources

import (
	"context"
	"net/http"
	"strings"
)

// Source is a stateful token provider. It may require an
// interactive login flow (OAuth via browser) or just refresh a
// pre-seeded token (RemoteStore). Each Source declares its own
// callback handling; a single SourceRegistry dispatches
// /auth/callback requests to the owning Source by state prefix.
//
// Every Source also implements TokenStore — callers that only
// need a token can keep using auth.Transport(src, base).
type Source interface {
	// Name is the registry key (matches config.AuthSource.Name).
	Name() string

	// Token returns a valid access token. Blocks until the first
	// login completes for OAuth Sources.
	Token(ctx context.Context) (string, error)

	// Login kicks off the browser flow (writes login URL to log,
	// optionally opens browser). No-op for token-mode Sources.
	Login(ctx context.Context) error

	// OwnsState reports whether the given OAuth state parameter
	// belongs to this Source. Default convention is a prefix match
	// of "<Name>." — StateOwnedBy helps implementers follow it.
	OwnsState(state string) bool

	// HandleCallback completes the OAuth flow for a callback
	// request that OwnsState returned true for. Sources that don't
	// participate in browser flows return a 400 from this method.
	HandleCallback(w http.ResponseWriter, r *http.Request)
}

// LoginHandler is implemented by Sources that handle the browser
// login request. Registry delegates to it when the LoginPath
// route fires.
type LoginHandler interface {
	HandleLogin(w http.ResponseWriter, r *http.Request)
}

// EncodeState returns a state parameter scoped to a Source by
// prefixing the random nonce with the Source name. The dispatcher
// reads the prefix to route the callback.
func EncodeState(name, nonce string) string {
	return name + "." + nonce
}

// StateOwnedBy reports whether a state belongs to the named Source
// under the default "<name>." encoding. Sources may call it from
// their OwnsState implementations.
func StateOwnedBy(name, state string) bool {
	return strings.HasPrefix(state, name+".")
}
