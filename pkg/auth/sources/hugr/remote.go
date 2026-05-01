package hugr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// RemoteStore exchanges an expired access token for a new one via an external
// token provider URL.
//
// The provider itself handles refresh logic — it always holds a valid token.
// If the provider hasn't refreshed yet, it may return the same (old) token,
// so RemoteStore retries with backoff: 5s, 30s, 150s.
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

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

// NewRemoteStore creates a RemoteStore with the initial access token
// and the URL of the token exchange service.
func NewRemoteStore(name, accessToken, tokenURL string) *RemoteStore {
	return &RemoteStore{
		name:      name,
		tokenURL:  tokenURL,
		token:     accessToken,
		expiresAt: time.Now().Add(30 * time.Second), // use initial token briefly before first refresh
		client:    &http.Client{Timeout: 10 * time.Second},
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
		name:     name,
		tokenURL: tokenURL,
		token:    bootstrap,
		// expiresAt zero (= epoch) → first Token() falls through to refresh.
		client: &http.Client{Timeout: 10 * time.Second},
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

// refresh exchanges the expired token for a new one.
// Retries up to 3 times (5s, 30s, 150s) if the provider returns the same token.
func (s *RemoteStore) refresh(ctx context.Context) (string, error) {
	oldToken := s.token
	backoff := []time.Duration{5 * time.Second, 30 * time.Second, 150 * time.Second}

	for attempt, wait := range backoff {
		newToken, expiresIn, err := s.exchange(ctx, oldToken)
		if err != nil {
			return "", err // 401/403 — fatal, no retry
		}

		if newToken != oldToken {
			s.token = newToken
			s.expiresAt = time.Now().Add(time.Duration(expiresIn) * time.Second)
			return s.token, nil
		}

		// Provider returned the same token — hasn't refreshed yet, wait and retry.
		if attempt < len(backoff)-1 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(wait):
			}
		}
	}

	return "", fmt.Errorf("token exchange: provider did not return a new token after %d retries", len(backoff))
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
		return "", 0, fmt.Errorf("token exchange: %d — credentials rejected", resp.StatusCode)
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
