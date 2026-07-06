package hugr

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// errExchangeRejected marks a 401/403 from the exchange endpoint — the
// presented credential is genuinely dead. Every other exchange failure
// (network, 5xx) is transient and retried on the backoff schedule: the hub
// deliberately answers 503 for store hiccups so a booting container can
// ride them out.
var errExchangeRejected = errors.New("credentials rejected")

// RemoteStore exchanges an expired access token for a new one via an external
// token provider URL.
//
// The provider itself handles refresh logic — it always holds a valid token.
// If the provider hasn't refreshed yet, it may return the same (old) token;
// transient endpoint failures and same-token responses are retried with
// backoff: 5s, 30s, 150s. A 401/403 is terminal (see errExchangeRejected),
// with one exception: when the rejected credential came from the token
// cache, the store invalidates the cache and falls back to the
// constructor-supplied credential once — so a stale cache can never
// permanently shadow a freshly-minted bootstrap secret.
//
// Implements Source: Token refreshes via the exchange URL; Login is a
// no-op (there's no browser flow); OwnsState always returns false and
// HandleCallback returns 400 — the SourceRegistry dispatcher will never
// route a callback here because no state is ever issued with the
// RemoteStore name as prefix.
type RemoteStore struct {
	name     string
	tokenURL string
	client   *http.Client

	// bootstrapCred is the constructor-supplied credential (env). Kept so
	// a cache-loaded token that turns out to be dead can fall back to it.
	bootstrapCred string

	// persistPath, when set, is where the CURRENT token is cached on disk
	// (0600) after every successful exchange — the persisted-token fast
	// path (spec-hub-side §1.5): a restarted container whose one-shot
	// bootstrap secret is already consumed still refreshes, because a
	// signature-valid (even expired) JWT is itself the exchange credential.
	persistPath string

	// backoff is the retry schedule for transient exchange failures and
	// same-token responses. Overridable in tests.
	backoff []time.Duration

	mu        sync.Mutex
	token     string
	expiresAt time.Time
	// tokenFromCache marks that token was loaded from the cache file and
	// has not yet been proven by a successful exchange — the fallback in
	// refresh() only triggers for such tokens.
	tokenFromCache bool
}

func defaultBackoff() []time.Duration {
	return []time.Duration{5 * time.Second, 30 * time.Second, 150 * time.Second}
}

// NewRemoteStore creates a RemoteStore with the initial access token
// and the URL of the token exchange service.
func NewRemoteStore(name, accessToken, tokenURL string) *RemoteStore {
	return &RemoteStore{
		name:          name,
		tokenURL:      tokenURL,
		token:         accessToken,
		bootstrapCred: accessToken,
		expiresAt:     time.Now().Add(30 * time.Second), // use initial token briefly before first refresh
		client:        &http.Client{Timeout: 10 * time.Second},
		backoff:       defaultBackoff(),
	}
}

// NewRemoteStoreBootstrap is like NewRemoteStore but treats the
// initial access token as a one-shot bootstrap secret rather than a
// usable bearer. The first Token() call triggers an immediate
// exchange — there is no "use the initial token briefly" window.
//
// hugr-query uses this constructor: HUGR_ACCESS_TOKEN is a random
// secret that the agent's /api/auth/agent-token endpoint redeems
// for the agent's actual Hugr JWT, and using the secret directly
// against Hugr would 401 every call.
func NewRemoteStoreBootstrap(name, bootstrap, tokenURL string) *RemoteStore {
	return &RemoteStore{
		name:          name,
		tokenURL:      tokenURL,
		token:         bootstrap,
		bootstrapCred: bootstrap,
		// expiresAt zero (= epoch) → first Token() falls through to refresh.
		client:  &http.Client{Timeout: 10 * time.Second},
		backoff: defaultBackoff(),
	}
}

// WithTokenCache enables the persisted-token fast path: the current token is
// written to path after every successful exchange, and read back here. A
// cached token — even one whose expiry has passed — replaces the initial
// credential, so the env bootstrap secret is only ever needed on the very
// first boot.
//
// The cache is KEYED to this chain: it records the token URL and a hash of
// the credential that bootstrapped it, and a file that does not match both
// is ignored. Without the key, two agents sharing a state dir (or an
// operator switching HUGR_ACCESS_TOKEN to a different agent) would silently
// run under the previous agent's identity. Load failures are silent by
// design — the cache is an optimization, the constructor-supplied credential
// still works without it.
func (s *RemoteStore) WithTokenCache(path string) *RemoteStore {
	s.persistPath = path
	b, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	var cached tokenCacheFile
	if json.Unmarshal(b, &cached) != nil || cached.Token == "" {
		return s
	}
	if cached.TokenURL != s.tokenURL || cached.CredentialSHA != credentialSHA(s.bootstrapCred) {
		return s // foreign cache (different endpoint or different agent credential)
	}
	s.token = cached.Token
	s.expiresAt = cached.ExpiresAt
	s.tokenFromCache = true
	return s
}

// CacheLoaded reports whether the current token came from the cache file
// (vs the constructor credential). For boot-time logging only.
func (s *RemoteStore) CacheLoaded() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tokenFromCache
}

// tokenCacheFile is the JSON shape persisted at persistPath.
type tokenCacheFile struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	// TokenURL + CredentialSHA key the cache to one exchange chain — see
	// WithTokenCache.
	TokenURL      string `json:"token_url"`
	CredentialSHA string `json:"credential_sha256"`
}

func credentialSHA(cred string) string {
	sum := sha256.Sum256([]byte(cred))
	return hex.EncodeToString(sum[:])
}

// persistLocked writes the current token to persistPath (0600, atomic via
// temp+rename so a crash never leaves a torn file). Caller holds s.mu.
// Best-effort: an unwritable cache must not fail the refresh that produced
// a perfectly good token.
func (s *RemoteStore) persistLocked() {
	if s.persistPath == "" {
		return
	}
	b, err := json.Marshal(tokenCacheFile{
		Token:         s.token,
		ExpiresAt:     s.expiresAt,
		TokenURL:      s.tokenURL,
		CredentialSHA: credentialSHA(s.bootstrapCred),
	})
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(s.persistPath), 0o700)
	tmp := s.persistPath + ".tmp"
	if os.WriteFile(tmp, b, 0o600) != nil {
		return
	}
	_ = os.Rename(tmp, s.persistPath)
}

// invalidateCacheLocked removes a cache file whose token the endpoint
// rejected. Caller holds s.mu.
func (s *RemoteStore) invalidateCacheLocked() {
	if s.persistPath != "" {
		_ = os.Remove(s.persistPath)
	}
}

// Name implements Source.
func (s *RemoteStore) Name() string { return s.name }

// Login is a no-op: RemoteStore refreshes via HTTP exchange, no browser flow.
func (s *RemoteStore) Login(_ context.Context) error { return nil }

// OwnsState always returns false — RemoteStore never participates in
// OAuth callback dispatch.
func (s *RemoteStore) OwnsState(string) bool { return false }

// HandleCallback rejects any callback — RemoteStore is never an owner.
func (s *RemoteStore) HandleCallback(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "remote store has no OAuth callback", http.StatusBadRequest)
}

func (s *RemoteStore) Token(ctx context.Context) (string, error) {
	tok, _, err := s.tokenWithTTL(ctx)
	return tok, err
}

// TokenWithTTL is like Token but also reports remaining seconds
// until the cached token's expiry. Callers that propagate the
// token to a downstream agent-token endpoint use this so the
// downstream RemoteStore caches for the JWT's actual lifetime.
func (s *RemoteStore) TokenWithTTL(ctx context.Context) (string, int, error) {
	return s.tokenWithTTL(ctx)
}

func (s *RemoteStore) tokenWithTTL(ctx context.Context) (string, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Refresh ~30s before expiry so a token that's "still valid"
	// by our clock isn't already rejected by Hugr (clock skew /
	// short server-side TTLs). This matches the OIDC source's
	// behaviour and avoids tail-end "token expired" errors that
	// look like an unrecoverable failure but are actually just
	// timing.
	if s.token != "" && time.Now().Add(30*time.Second).Before(s.expiresAt) {
		return s.token, ttlSeconds(s.expiresAt), nil
	}

	tok, err := s.refresh(ctx)
	if err != nil {
		return "", 0, err
	}
	return tok, ttlSeconds(s.expiresAt), nil
}

func ttlSeconds(expiresAt time.Time) int {
	d := time.Until(expiresAt)
	if d <= 0 {
		return 0
	}
	return int(d / time.Second)
}

// refresh exchanges the current credential for a fresh token. Caller holds
// s.mu.
//
//   - success (new token) → persist to cache, done.
//   - 401/403 → terminal, EXCEPT when the rejected credential was loaded
//     from the cache: then the cache is invalidated and the constructor
//     credential is tried once (a stale cache must not shadow a fresh
//     bootstrap secret).
//   - transient failure (network, 5xx) or same-token response → retry on
//     the backoff schedule (the hub answers 503 for store hiccups exactly
//     so boots survive them).
func (s *RemoteStore) refresh(ctx context.Context) (string, error) {
	waits := 0
	for {
		newToken, expiresIn, err := s.exchange(ctx, s.token)
		switch {
		case errors.Is(err, errExchangeRejected):
			if s.tokenFromCache && s.bootstrapCred != "" && s.token != s.bootstrapCred {
				// The cached token is dead (key rotation, foreign env, hard
				// revocation). Drop it and retry immediately with the
				// credential the operator actually configured.
				s.invalidateCacheLocked()
				s.token = s.bootstrapCred
				s.tokenFromCache = false
				continue
			}
			return "", err

		case err == nil && newToken != s.token:
			s.token = newToken
			s.expiresAt = time.Now().Add(time.Duration(expiresIn) * time.Second)
			s.tokenFromCache = false
			s.persistLocked()
			return s.token, nil
		}

		// Transient failure or same-token response — wait and retry.
		if waits >= len(s.backoff) {
			if err != nil {
				return "", err
			}
			return "", fmt.Errorf("token exchange: provider did not return a new token after %d retries", len(s.backoff))
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(s.backoff[waits]):
		}
		waits++
	}
}

type exchangeRequest struct {
	Token string `json:"token"`
}

type exchangeResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	Error       string `json:"error,omitempty"`
}

func (s *RemoteStore) exchange(ctx context.Context, expiredToken string) (string, int, error) {
	body, _ := json.Marshal(exchangeRequest{Token: expiredToken})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.tokenURL, bytes.NewReader(body))
	if err != nil {
		return "", 0, fmt.Errorf("token exchange: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("token exchange: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Check status before decoding — non-200 may return HTML, not JSON.
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", 0, fmt.Errorf("token exchange: %d — %w", resp.StatusCode, errExchangeRejected)
	}
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("token exchange: unexpected status %d", resp.StatusCode)
	}

	var result exchangeResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return "", 0, fmt.Errorf("token exchange: decode response: %w", err)
	}
	if result.AccessToken == "" {
		return "", 0, fmt.Errorf("token exchange: empty access_token in response")
	}
	return result.AccessToken, result.ExpiresIn, nil
}
