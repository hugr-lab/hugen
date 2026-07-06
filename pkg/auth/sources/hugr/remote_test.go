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
// credential each exchange presented.
func exchangeServer(t *testing.T, issue string) (*httptest.Server, *[]string) {
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
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": issue,
			"expires_in":   3600,
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

// Happy path: a cached token with future expiry is served directly — no
// exchange, and the env bootstrap secret is never used.
func TestWithTokenCache_ServesCachedToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hugr-token.json")
	writeCache(t, path, "cached-jwt", time.Now().Add(time.Hour))
	srv, seen := exchangeServer(t, "fresh-jwt")

	s := NewRemoteStoreBootstrap("hugr", "bootstrap-secret", srv.URL).WithTokenCache(path)
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
	writeCache(t, path, "expired-jwt", time.Now().Add(-time.Hour))
	srv, seen := exchangeServer(t, "fresh-jwt")

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
	srv, seen := exchangeServer(t, "fresh-jwt")

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
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("cache mode = %v, want 0600", info.Mode().Perm())
		}
	}

	// Second store on the same path: cached fresh-jwt served, no exchange.
	s2 := NewRemoteStoreBootstrap("hugr", "other-secret", srv.URL).WithTokenCache(path)
	tok, err := s2.Token(context.Background())
	if err != nil {
		t.Fatalf("Token (second store): %v", err)
	}
	if tok != "fresh-jwt" || len(*seen) != 1 {
		t.Fatalf("second store token=%q exchanges=%d, want fresh-jwt / 1", tok, len(*seen))
	}
}

// A corrupt cache file must not break the store — env credential still works.
func TestWithTokenCache_CorruptFileFallsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hugr-token.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, seen := exchangeServer(t, "fresh-jwt")

	s := NewRemoteStoreBootstrap("hugr", "bootstrap-secret", srv.URL).WithTokenCache(path)
	tok, err := s.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "fresh-jwt" || len(*seen) != 1 || (*seen)[0] != "bootstrap-secret" {
		t.Fatalf("token=%q credentials=%v, want fresh-jwt via bootstrap-secret", tok, *seen)
	}
}

func writeCache(t *testing.T, path, token string, expiresAt time.Time) {
	t.Helper()
	b, err := json.Marshal(tokenCacheFile{Token: token, ExpiresAt: expiresAt})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}
