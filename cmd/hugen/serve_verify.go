package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sync"
	"time"

	qeclient "github.com/hugr-lab/query-engine/client"

	"github.com/hugr-lab/hugen/pkg/adapter/httpapi"
	"github.com/hugr-lab/hugen/pkg/identity/hub"
	"github.com/hugr-lab/hugen/pkg/runtime"
)

// userTokenVerifier builds the H2 forwarded-user-token verifier. It asks hugr
// ITSELF (auth.me, via a per-token client) to validate the token and return the
// user's identity + role — hugr is the authority (D1). A token hugr accepts
// yields an authoritative WhoAmI; a rejected token yields an error → 401. The
// role is computed server-side by auth.me, so this also gets the role a bare
// JWKS signature check could not. Results are cached briefly so a burst from
// one user doesn't hammer auth.me.
func userTokenVerifier(core *runtime.Core) httpapi.VerifyFunc {
	hugrURL := core.Cfg.Hugr.URL
	cache := newVerifyCache(60 * time.Second)
	return func(ctx context.Context, rawToken string) (httpapi.VerifiedUser, error) {
		if u, ok := cache.get(rawToken); ok {
			return u, nil
		}
		qe := qeclient.NewClient(
			hugrURL+"/ipc",
			qeclient.WithTransport(bearerRT{token: rawToken, base: http.DefaultTransport}),
		)
		who, err := hub.New(qe).WhoAmI(ctx)
		if err != nil {
			return httpapi.VerifiedUser{}, err
		}
		u := httpapi.VerifiedUser{UserID: who.UserID, Name: who.UserName, Role: who.Role}
		cache.put(rawToken, u)
		return u, nil
	}
}

// bearerRT injects a static Authorization: Bearer <token> — a per-user hugr
// client transport (contrast auth.Transport, which pulls the AGENT token from a
// store).
type bearerRT struct {
	token string
	base  http.RoundTripper
}

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

// verifyCache is a tiny TTL cache keyed by a hash of the token, so a request
// burst from one user resolves to one auth.me round-trip. A hash key avoids
// keeping raw tokens as map keys.
type verifyCache struct {
	ttl time.Duration
	mu  sync.Mutex
	m   map[string]verifyEntry
}

type verifyEntry struct {
	user    httpapi.VerifiedUser
	expires time.Time
}

func newVerifyCache(ttl time.Duration) *verifyCache {
	return &verifyCache{ttl: ttl, m: map[string]verifyEntry{}}
}

func (c *verifyCache) key(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

func (c *verifyCache) get(tok string) (httpapi.VerifiedUser, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[c.key(tok)]
	if !ok || time.Now().After(e.expires) {
		return httpapi.VerifiedUser{}, false
	}
	return e.user, true
}

func (c *verifyCache) put(tok string, u httpapi.VerifiedUser) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[c.key(tok)] = verifyEntry{user: u, expires: time.Now().Add(c.ttl)}
}
