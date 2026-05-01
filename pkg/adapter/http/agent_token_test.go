package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSource is an AgentTokenSource that returns a deterministic
// rotating token sequence — the test asserts the handler's behaviour
// across rotations without depending on a real OIDC store.
type fakeSource struct {
	mu     sync.Mutex
	tokens []string
	calls  atomic.Int64
	errs   []error // optional: per-call errors
}

func (f *fakeSource) Token(_ context.Context) (string, int, error) {
	idx := int(f.calls.Add(1) - 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if idx < len(f.errs) && f.errs[idx] != nil {
		return "", 0, f.errs[idx]
	}
	if idx >= len(f.tokens) {
		// repeat last token (typical "no rotation yet" case)
		return f.tokens[len(f.tokens)-1], 600, nil
	}
	return f.tokens[idx], 600, nil
}

func newAgentTokenStoreForTest(t *testing.T, source AgentTokenSource, opts AgentTokenOptions) *AgentTokenStore {
	t.Helper()
	store, err := NewAgentTokenStore(source, opts)
	if err != nil {
		t.Fatalf("NewAgentTokenStore: %v", err)
	}
	return store
}

func postJSON(t *testing.T, store *AgentTokenStore, body string) (int, agentTokenResponse) {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/auth/agent-token", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:43210"
	rec := httptest.NewRecorder()
	store.Handle(rec, req)
	var resp agentTokenResponse
	if rec.Code == 200 {
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
	}
	return rec.Code, resp
}

func TestAgentToken_BootstrapWindow(t *testing.T) {
	src := &fakeSource{tokens: []string{"jwt-1"}}
	store := newAgentTokenStoreForTest(t, src, AgentTokenOptions{BootstrapWindow: 50 * time.Millisecond})

	revoke, err := store.RegisterSpawn("boot-1")
	if err != nil {
		t.Fatalf("RegisterSpawn: %v", err)
	}
	defer revoke()

	// Within window: success.
	code, resp := postJSON(t, store, `{"token":"boot-1"}`)
	if code != 200 {
		t.Fatalf("first exchange: code=%d", code)
	}
	if resp.AccessToken != "jwt-1" {
		t.Fatalf("first exchange: token=%q want jwt-1", resp.AccessToken)
	}
	if resp.TokenType != "Bearer" {
		t.Fatalf("first exchange: token_type=%q want Bearer", resp.TokenType)
	}

	// Sleep past the window — re-using the bootstrap is now denied.
	// (The previously-issued jwt-1 still works via IssuedHistory.)
	time.Sleep(70 * time.Millisecond)
	code, _ = postJSON(t, store, `{"token":"boot-1"}`)
	if code != 401 {
		t.Fatalf("post-window bootstrap: code=%d want 401", code)
	}
}

func TestAgentToken_IssuedHistoryRotation(t *testing.T) {
	src := &fakeSource{tokens: []string{"jwt-1", "jwt-2", "jwt-3"}}
	store := newAgentTokenStoreForTest(t, src, AgentTokenOptions{HistorySize: 4})

	revoke, _ := store.RegisterSpawn("boot-X")
	defer revoke()

	// Bootstrap → jwt-1
	_, r1 := postJSON(t, store, `{"token":"boot-X"}`)
	if r1.AccessToken != "jwt-1" {
		t.Fatalf("r1=%q", r1.AccessToken)
	}

	// Refresh with previous jwt-1 → jwt-2
	_, r2 := postJSON(t, store, `{"token":"jwt-1"}`)
	if r2.AccessToken != "jwt-2" {
		t.Fatalf("r2=%q", r2.AccessToken)
	}

	// Refresh with previous jwt-2 → jwt-3
	_, r3 := postJSON(t, store, `{"token":"jwt-2"}`)
	if r3.AccessToken != "jwt-3" {
		t.Fatalf("r3=%q", r3.AccessToken)
	}

	// Re-using an old issued token still works while it's in history.
	code, _ := postJSON(t, store, `{"token":"jwt-1"}`)
	if code != 200 {
		t.Fatalf("re-use jwt-1 within history: code=%d want 200", code)
	}
}

func TestAgentToken_HistoryEvictionDropsToken(t *testing.T) {
	src := &fakeSource{tokens: []string{"a", "b", "c", "d", "e"}}
	store := newAgentTokenStoreForTest(t, src, AgentTokenOptions{HistorySize: 2})

	_, _ = store.RegisterSpawn("boot-Y")

	// Issue tokens a, b, c, d via successive exchanges.
	prev := "boot-Y"
	for _, want := range []string{"a", "b", "c", "d"} {
		_, resp := postJSON(t, store, `{"token":"`+prev+`"}`)
		if resp.AccessToken != want {
			t.Fatalf("expected %q got %q", want, resp.AccessToken)
		}
		prev = resp.AccessToken
	}
	// History size 2 → "a" and "b" must have evicted; reusing them returns 401.
	for _, evicted := range []string{"a", "b"} {
		code, _ := postJSON(t, store, `{"token":"`+evicted+`"}`)
		if code != 401 {
			t.Fatalf("evicted %q: code=%d want 401", evicted, code)
		}
	}
	// "c" and "d" still in history.
	for _, live := range []string{"c", "d"} {
		code, _ := postJSON(t, store, `{"token":"`+live+`"}`)
		if code != 200 {
			t.Fatalf("live %q: code=%d want 200", live, code)
		}
	}
}

func TestAgentToken_CrossSpawnIsolation(t *testing.T) {
	src := &fakeSource{tokens: []string{"jwt-A", "jwt-A", "jwt-B"}}
	store := newAgentTokenStoreForTest(t, src, AgentTokenOptions{})

	// Two spawns; revoking one drops its issued history.
	revoke1, _ := store.RegisterSpawn("boot-1")
	revoke2, _ := store.RegisterSpawn("boot-2")
	defer revoke2()

	_, r1 := postJSON(t, store, `{"token":"boot-1"}`)
	if r1.AccessToken == "" {
		t.Fatalf("spawn 1 first exchange empty")
	}
	_, _ = postJSON(t, store, `{"token":"boot-2"}`)

	// Revoke spawn 1 — its issued tokens must no longer authenticate.
	revoke1()
	code, _ := postJSON(t, store, `{"token":"`+r1.AccessToken+`"}`)
	if code != 401 {
		t.Fatalf("revoked spawn 1 token: code=%d want 401", code)
	}
	// Spawn 2 is unaffected.
	code, _ = postJSON(t, store, `{"token":"boot-2"}`)
	// boot-2 was already used — re-using bootstrap within window is fine.
	if code != 200 {
		t.Fatalf("spawn 2 bootstrap re-use: code=%d want 200", code)
	}
}

func TestAgentToken_UnknownTokenIs401(t *testing.T) {
	src := &fakeSource{tokens: []string{"jwt-1"}}
	store := newAgentTokenStoreForTest(t, src, AgentTokenOptions{})
	code, _ := postJSON(t, store, `{"token":"never-issued"}`)
	if code != 401 {
		t.Fatalf("unknown token: code=%d want 401", code)
	}
}

func TestAgentToken_NonLoopbackForbidden(t *testing.T) {
	src := &fakeSource{tokens: []string{"jwt-1"}}
	store := newAgentTokenStoreForTest(t, src, AgentTokenOptions{})
	_, _ = store.RegisterSpawn("boot-Z")

	req := httptest.NewRequest("POST", "/api/auth/agent-token", strings.NewReader(`{"token":"boot-Z"}`))
	req.RemoteAddr = "10.0.0.1:5000"
	rec := httptest.NewRecorder()
	store.Handle(rec, req)
	if rec.Code != 403 {
		t.Fatalf("non-loopback: code=%d want 403", rec.Code)
	}
}

func TestAgentToken_GetIs405(t *testing.T) {
	src := &fakeSource{tokens: []string{"jwt-1"}}
	store := newAgentTokenStoreForTest(t, src, AgentTokenOptions{})
	req := httptest.NewRequest("GET", "/api/auth/agent-token", nil)
	req.RemoteAddr = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	store.Handle(rec, req)
	if rec.Code != 405 {
		t.Fatalf("GET: code=%d want 405", rec.Code)
	}
}

func TestAgentToken_BadJSONIs400(t *testing.T) {
	src := &fakeSource{tokens: []string{"jwt-1"}}
	store := newAgentTokenStoreForTest(t, src, AgentTokenOptions{})
	req := httptest.NewRequest("POST", "/api/auth/agent-token", strings.NewReader(`not json`))
	req.RemoteAddr = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	store.Handle(rec, req)
	if rec.Code != 400 {
		t.Fatalf("bad JSON: code=%d want 400", rec.Code)
	}
}

func TestAgentToken_SourceErrorIs502(t *testing.T) {
	src := &fakeSource{tokens: []string{"jwt-1"}, errs: []error{errors.New("source down")}}
	store := newAgentTokenStoreForTest(t, src, AgentTokenOptions{})
	_, _ = store.RegisterSpawn("boot-E")
	code, _ := postJSON(t, store, `{"token":"boot-E"}`)
	if code != 502 {
		t.Fatalf("source error: code=%d want 502", code)
	}
}

func TestAgentToken_NilSourceRejected(t *testing.T) {
	if _, err := NewAgentTokenStore(nil, AgentTokenOptions{}); err == nil {
		t.Fatalf("NewAgentTokenStore(nil) should fail")
	}
}

func TestAgentToken_DuplicateBootstrapRejected(t *testing.T) {
	src := &fakeSource{tokens: []string{"jwt-1"}}
	store := newAgentTokenStoreForTest(t, src, AgentTokenOptions{})
	_, err := store.RegisterSpawn("boot-D")
	if err != nil {
		t.Fatalf("first RegisterSpawn: %v", err)
	}
	if _, err := store.RegisterSpawn("boot-D"); err == nil {
		t.Fatalf("duplicate bootstrap should fail")
	}
}

func TestAgentToken_RevokeIdempotent(t *testing.T) {
	src := &fakeSource{tokens: []string{"jwt-1"}}
	store := newAgentTokenStoreForTest(t, src, AgentTokenOptions{})
	revoke, _ := store.RegisterSpawn("boot-R")
	revoke()
	revoke() // must not panic
	store.RevokeSpawn("never-registered")
	if store.SpawnCount() != 0 {
		t.Fatalf("SpawnCount=%d want 0", store.SpawnCount())
	}
}
