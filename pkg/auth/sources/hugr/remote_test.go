package hugr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// exchangeServer is a minimal token endpoint implementing the RemoteStore
// contract: POST {token} → {access_token, expires_in}. It records the
// credential each exchange presented. respond, when set, overrides the
// default always-issue behaviour.
func exchangeServer(t *testing.T, issue string, respond func(cred string, n int, w http.ResponseWriter) bool) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		seen = append(seen, req.Token)
		if respond != nil && respond(req.Token, len(seen), w) {
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": issue,
			"expires_in":   3600,
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func writeCache(t *testing.T, path, token string, expiresAt time.Time, tokenURL, cred string) {
	t.Helper()
	b, err := json.Marshal(tokenCacheFile{
		Token:         token,
		ExpiresAt:     expiresAt,
		TokenURL:      tokenURL,
		CredentialSHA: credentialSHA(cred),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Happy path: a cached token with future expiry is served directly — no
// exchange, and the env bootstrap secret is never used.
func TestWithTokenCache_ServesCachedToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hugr-token.json")
	srv, seen := exchangeServer(t, "fresh-jwt", nil)
	writeCache(t, path, "cached-jwt", time.Now().Add(time.Hour), srv.URL, "bootstrap-secret")

	s := NewRemoteStoreBootstrap("hugr", "bootstrap-secret", srv.URL).WithTokenCache(path)
	if !s.CacheLoaded() {
		t.Fatal("cache should have loaded")
	}
	tok, err := s.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "cached-jwt" {
		t.Fatalf("token = %q, want cached-jwt", tok)
	}
	if len(*seen) != 0 {
		t.Fatalf("exchange called %d times, want 0", len(*seen))
	}
}

// The fast path's whole point: an EXPIRED cached token is still the exchange
// credential (its signature is the proof), so a restarted container whose
// one-shot bootstrap secret is consumed keeps refreshing.
func TestWithTokenCache_ExpiredCachedTokenIsTheCredential(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hugr-token.json")
	srv, seen := exchangeServer(t, "fresh-jwt", nil)
	writeCache(t, path, "expired-jwt", time.Now().Add(-time.Hour), srv.URL, "bootstrap-secret")

	s := NewRemoteStoreBootstrap("hugr", "bootstrap-secret", srv.URL).WithTokenCache(path)
	tok, err := s.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "fresh-jwt" {
		t.Fatalf("token = %q, want fresh-jwt", tok)
	}
	if len(*seen) != 1 || (*seen)[0] != "expired-jwt" {
		t.Fatalf("exchange credentials = %v, want [expired-jwt] (cached beats env secret)", *seen)
	}
}

// A successful exchange persists the new token (0600) so the next process
// picks it up; missing cache file falls back to the env bootstrap secret.
func TestWithTokenCache_PersistsAfterRefresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "hugr-token.json")
	srv, seen := exchangeServer(t, "fresh-jwt", nil)

	s := NewRemoteStoreBootstrap("hugr", "bootstrap-secret", srv.URL).WithTokenCache(path)
	if _, err := s.Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}
	if len(*seen) != 1 || (*seen)[0] != "bootstrap-secret" {
		t.Fatalf("exchange credentials = %v, want [bootstrap-secret]", *seen)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cache not written: %v", err)
	}
	var cached tokenCacheFile
	if err := json.Unmarshal(b, &cached); err != nil {
		t.Fatalf("cache unmarshal: %v", err)
	}
	if cached.Token != "fresh-jwt" || !cached.ExpiresAt.After(time.Now()) {
		t.Fatalf("cache = %+v, want fresh-jwt with future expiry", cached)
	}
	if cached.TokenURL != srv.URL || cached.CredentialSHA != credentialSHA("bootstrap-secret") {
		t.Fatalf("cache keying missing: %+v", cached)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("cache mode = %v, want 0600", info.Mode().Perm())
		}
	}

	// Second store, SAME credential (docker-restart shape): cache used.
	s2 := NewRemoteStoreBootstrap("hugr", "bootstrap-secret", srv.URL).WithTokenCache(path)
	tok, err := s2.Token(context.Background())
	if err != nil {
		t.Fatalf("Token (second store): %v", err)
	}
	if tok != "fresh-jwt" || len(*seen) != 1 {
		t.Fatalf("second store token=%q exchanges=%d, want fresh-jwt / 1", tok, len(*seen))
	}
}

// The cache is keyed to (token URL, bootstrap credential): a DIFFERENT
// credential must ignore the cache — otherwise two agents sharing a state
// dir, or an operator re-pointing HUGR_ACCESS_TOKEN at another agent, would
// silently run under the previous agent's identity.
func TestWithTokenCache_ForeignCacheIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hugr-token.json")
	srv, seen := exchangeServer(t, "fresh-jwt", nil)
	writeCache(t, path, "agent-A-jwt", time.Now().Add(time.Hour), srv.URL, "agent-A-secret")

	// Different credential → cache ignored, own secret exchanged.
	s := NewRemoteStoreBootstrap("hugr", "agent-B-secret", srv.URL).WithTokenCache(path)
	if s.CacheLoaded() {
		t.Fatal("foreign cache (different credential) must not load")
	}
	tok, err := s.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "fresh-jwt" || len(*seen) != 1 || (*seen)[0] != "agent-B-secret" {
		t.Fatalf("token=%q credentials=%v, want fresh-jwt via agent-B-secret", tok, *seen)
	}

	// Different token URL → also ignored.
	writeCache(t, path, "agent-A-jwt", time.Now().Add(time.Hour), "http://other-hub/token", "agent-A-secret")
	s2 := NewRemoteStoreBootstrap("hugr", "agent-A-secret", srv.URL).WithTokenCache(path)
	if s2.CacheLoaded() {
		t.Fatal("foreign cache (different token URL) must not load")
	}
}

// A cache-loaded token the endpoint REJECTS must not permanently shadow the
// env credential: the store invalidates the cache and retries once with the
// constructor secret (key rotation / stale cache recovery).
func TestWithTokenCache_RejectedCacheFallsBackToEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hugr-token.json")
	srv, seen := exchangeServer(t, "fresh-jwt", func(cred string, _ int, w http.ResponseWriter) bool {
		if cred == "stale-cached-jwt" {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "credential rejected"})
			return true
		}
		return false
	})
	writeCache(t, path, "stale-cached-jwt", time.Now().Add(-time.Minute), srv.URL, "fresh-secret")

	s := NewRemoteStoreBootstrap("hugr", "fresh-secret", srv.URL).WithTokenCache(path)
	tok, err := s.Token(context.Background())
	if err != nil {
		t.Fatalf("Token should recover via env credential: %v", err)
	}
	if tok != "fresh-jwt" {
		t.Fatalf("token = %q, want fresh-jwt", tok)
	}
	if len(*seen) != 2 || (*seen)[0] != "stale-cached-jwt" || (*seen)[1] != "fresh-secret" {
		t.Fatalf("exchange credentials = %v, want [stale-cached-jwt fresh-secret]", *seen)
	}
	// The dead cache was replaced by the fresh token's cache entry.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cache should be rewritten after recovery: %v", err)
	}
	var cached tokenCacheFile
	if err := json.Unmarshal(b, &cached); err != nil || cached.Token != "fresh-jwt" {
		t.Fatalf("cache after recovery = %+v (err=%v), want fresh-jwt", cached, err)
	}

	// A rejected ENV credential stays terminal — no infinite loop.
	srv2, _ := exchangeServer(t, "", func(_ string, _ int, w http.ResponseWriter) bool {
		w.WriteHeader(http.StatusUnauthorized)
		return true
	})
	s2 := NewRemoteStoreBootstrap("hugr", "dead-secret", srv2.URL)
	if _, err := s2.Token(context.Background()); err == nil {
		t.Fatal("rejected env credential must be terminal")
	}
}

// Transient endpoint failures (the hub answers 503 on store hiccups) are
// retried on the backoff schedule instead of aborting the boot.
func TestRefresh_TransientFailureRetries(t *testing.T) {
	srv, seen := exchangeServer(t, "fresh-jwt", func(_ string, n int, w http.ResponseWriter) bool {
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return true
		}
		return false
	})

	s := NewRemoteStoreBootstrap("hugr", "bootstrap-secret", srv.URL)
	s.backoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	tok, err := s.Token(context.Background())
	if err != nil {
		t.Fatalf("Token should survive a transient 503: %v", err)
	}
	if tok != "fresh-jwt" || len(*seen) != 2 {
		t.Fatalf("token=%q exchanges=%d, want fresh-jwt after 2 attempts", tok, len(*seen))
	}

	// A permanently-down endpoint still fails after the schedule runs out.
	srv2, seen2 := exchangeServer(t, "", func(_ string, _ int, w http.ResponseWriter) bool {
		w.WriteHeader(http.StatusServiceUnavailable)
		return true
	})
	s2 := NewRemoteStoreBootstrap("hugr", "bootstrap-secret", srv2.URL)
	s2.backoff = []time.Duration{time.Millisecond, time.Millisecond}
	if _, err := s2.Token(context.Background()); err == nil {
		t.Fatal("permanent 503 must eventually fail")
	}
	if len(*seen2) != 3 { // initial attempt + one per backoff slot
		t.Fatalf("exchanges = %d, want 3", len(*seen2))
	}
}

// A corrupt cache file must not break the store — env credential still works.
func TestWithTokenCache_CorruptFileFallsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hugr-token.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, seen := exchangeServer(t, "fresh-jwt", nil)

	s := NewRemoteStoreBootstrap("hugr", "bootstrap-secret", srv.URL).WithTokenCache(path)
	tok, err := s.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "fresh-jwt" || len(*seen) != 1 || (*seen)[0] != "bootstrap-secret" {
		t.Fatalf("token=%q credentials=%v, want fresh-jwt via bootstrap-secret", tok, *seen)
	}
}
