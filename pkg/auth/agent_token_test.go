package auth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeTTLSource is a Service primary candidate that exposes both
// the sources.Source surface (Name / Token / Login / OwnsState /
// HandleCallback) and the ttlAware extension (TokenWithTTL) — so
// attachAgentTokensLocked picks it up and the loopback handler can
// mint JWTs on every exchange.
type fakeTTLSource struct {
	name string

	mu     sync.Mutex
	tokens []string
	calls  atomic.Int64
	errs   []error
}

func (f *fakeTTLSource) Name() string { return f.name }
func (f *fakeTTLSource) Token(ctx context.Context) (string, error) {
	tok, _, err := f.TokenWithTTL(ctx)
	return tok, err
}
func (f *fakeTTLSource) Login(context.Context) error { return nil }
func (f *fakeTTLSource) OwnsState(string) bool       { return false }
func (f *fakeTTLSource) HandleCallback(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "no", http.StatusBadRequest)
}

func (f *fakeTTLSource) TokenWithTTL(_ context.Context) (string, int, error) {
	idx := int(f.calls.Add(1) - 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if idx < len(f.errs) && f.errs[idx] != nil {
		return "", 0, f.errs[idx]
	}
	if len(f.tokens) == 0 {
		return "jwt", 600, nil
	}
	if idx >= len(f.tokens) {
		return f.tokens[len(f.tokens)-1], 600, nil
	}
	return f.tokens[idx], 600, nil
}

// nonTTLSource is a primary candidate that does NOT implement
// ttlAware — used to verify Service skips the loopback wiring for
// non-hugr-flavoured primaries.
type nonTTLSource struct{ name string }

func (s *nonTTLSource) Name() string                          { return s.name }
func (s *nonTTLSource) Token(context.Context) (string, error) { return "x", nil }
func (s *nonTTLSource) Login(context.Context) error           { return nil }
func (s *nonTTLSource) OwnsState(string) bool                 { return false }
func (s *nonTTLSource) HandleCallback(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "no", http.StatusBadRequest)
}

func newTestService(t *testing.T) *Service {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewService(logger, http.NewServeMux(), "", 0, false)
}

func TestNewStdioAuth_IssuesDistinctBootstraps(t *testing.T) {
	svc := newTestService(t)
	if err := svc.AddPrimary(&fakeTTLSource{name: "hugr", tokens: []string{"jwt-1"}}); err != nil {
		t.Fatalf("AddPrimary: %v", err)
	}
	a, err := svc.NewStdioAuth(context.Background(), "hugr")
	if err != nil {
		t.Fatalf("NewStdioAuth a: %v", err)
	}
	t.Cleanup(a.RevokeFunc)
	b, err := svc.NewStdioAuth(context.Background(), "hugr")
	if err != nil {
		t.Fatalf("NewStdioAuth b: %v", err)
	}
	t.Cleanup(b.RevokeFunc)
	if a.BootstrapToken == "" || a.BootstrapToken == b.BootstrapToken {
		t.Fatalf("expected distinct non-empty bootstraps; got %q vs %q", a.BootstrapToken, b.BootstrapToken)
	}
	if !strings.HasSuffix(a.TokenURL, AgentTokenPath) {
		t.Fatalf("TokenURL %q does not end in %q", a.TokenURL, AgentTokenPath)
	}
	got := a.Env()
	if got["HUGR_TOKEN_URL"] != a.TokenURL {
		t.Fatalf("Env HUGR_TOKEN_URL = %q want %q", got["HUGR_TOKEN_URL"], a.TokenURL)
	}
	if got["HUGR_ACCESS_TOKEN"] != a.BootstrapToken {
		t.Fatalf("Env HUGR_ACCESS_TOKEN = %q want %q", got["HUGR_ACCESS_TOKEN"], a.BootstrapToken)
	}
}

func TestNewStdioAuth_NonPrimaryRejected(t *testing.T) {
	svc := newTestService(t)
	if err := svc.AddPrimary(&fakeTTLSource{name: "hugr"}); err != nil {
		t.Fatalf("AddPrimary: %v", err)
	}
	if err := svc.Add(&fakeTTLSource{name: "other"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := svc.NewStdioAuth(context.Background(), "other"); err == nil ||
		!strings.Contains(err.Error(), "primary") {
		t.Fatalf("expected primary-only error, got %v", err)
	}
}

func TestNewStdioAuth_EmptyName(t *testing.T) {
	svc := newTestService(t)
	if err := svc.AddPrimary(&fakeTTLSource{name: "hugr"}); err != nil {
		t.Fatalf("AddPrimary: %v", err)
	}
	if _, err := svc.NewStdioAuth(context.Background(), ""); err == nil {
		t.Fatal("expected error for empty name")
	}
}

func TestNewStdioAuth_NoPrimary_NoLoopback(t *testing.T) {
	svc := newTestService(t)
	if err := svc.Add(&nonTTLSource{name: "static"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := svc.NewStdioAuth(context.Background(), "static"); err == nil ||
		!strings.Contains(err.Error(), "primary") {
		t.Fatalf("expected primary-only error, got %v", err)
	}
}

func TestNewStdioAuth_NonTTLPrimary_NoLoopback(t *testing.T) {
	svc := newTestService(t)
	if err := svc.AddPrimary(&nonTTLSource{name: "static"}); err != nil {
		t.Fatalf("AddPrimary: %v", err)
	}
	if _, err := svc.NewStdioAuth(context.Background(), "static"); err == nil ||
		!strings.Contains(err.Error(), "agent-token store not configured") {
		t.Fatalf("expected store-not-configured error, got %v", err)
	}
}

// agentTokenServer mounts the Service's mux so we can hit the
// loopback handler directly. Returns the URL the path is served at.
func agentTokenServer(t *testing.T, svc *Service) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(svc.mux)
	t.Cleanup(srv.Close)
	return srv
}

func postExchange(t *testing.T, srv *httptest.Server, body string) (int, agentTokenResponse) {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+AgentTokenPath, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:43210"
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	var tr agentTokenResponse
	if resp.StatusCode == 200 {
		if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return resp.StatusCode, tr
}

func TestAgentTokenHandler_BootstrapExchangesForJWT(t *testing.T) {
	svc := newTestService(t)
	if err := svc.AddPrimary(&fakeTTLSource{name: "hugr", tokens: []string{"jwt-1", "jwt-2"}}); err != nil {
		t.Fatalf("AddPrimary: %v", err)
	}
	sa, err := svc.NewStdioAuth(context.Background(), "hugr")
	if err != nil {
		t.Fatalf("NewStdioAuth: %v", err)
	}
	t.Cleanup(sa.RevokeFunc)

	srv := agentTokenServer(t, svc)
	code, r := postExchange(t, srv, `{"token":"`+sa.BootstrapToken+`"}`)
	if code != 200 {
		t.Fatalf("first exchange code=%d", code)
	}
	if r.AccessToken != "jwt-1" {
		t.Fatalf("AccessToken=%q want jwt-1", r.AccessToken)
	}
	if r.TokenType != "Bearer" {
		t.Fatalf("TokenType=%q want Bearer", r.TokenType)
	}
	if r.ExpiresIn != 600 {
		t.Fatalf("ExpiresIn=%d want 600", r.ExpiresIn)
	}

	// Rotate using the previously-issued JWT.
	code, r = postExchange(t, srv, `{"token":"jwt-1"}`)
	if code != 200 || r.AccessToken != "jwt-2" {
		t.Fatalf("rotate code=%d AccessToken=%q", code, r.AccessToken)
	}
}

func TestAgentTokenHandler_RevokeBlocksFollowup(t *testing.T) {
	svc := newTestService(t)
	if err := svc.AddPrimary(&fakeTTLSource{name: "hugr", tokens: []string{"jwt-1"}}); err != nil {
		t.Fatalf("AddPrimary: %v", err)
	}
	sa, err := svc.NewStdioAuth(context.Background(), "hugr")
	if err != nil {
		t.Fatalf("NewStdioAuth: %v", err)
	}

	srv := agentTokenServer(t, svc)
	code, r := postExchange(t, srv, `{"token":"`+sa.BootstrapToken+`"}`)
	if code != 200 {
		t.Fatalf("pre-revoke code=%d", code)
	}

	sa.RevokeFunc()

	code, _ = postExchange(t, srv, `{"token":"`+sa.BootstrapToken+`"}`)
	if code != 401 {
		t.Fatalf("post-revoke bootstrap code=%d want 401", code)
	}
	code, _ = postExchange(t, srv, `{"token":"`+r.AccessToken+`"}`)
	if code != 401 {
		t.Fatalf("post-revoke issued code=%d want 401", code)
	}
}

func TestAgentTokenHandler_UnknownTokenIs401(t *testing.T) {
	svc := newTestService(t)
	if err := svc.AddPrimary(&fakeTTLSource{name: "hugr"}); err != nil {
		t.Fatalf("AddPrimary: %v", err)
	}
	srv := agentTokenServer(t, svc)
	code, _ := postExchange(t, srv, `{"token":"never-issued"}`)
	if code != 401 {
		t.Fatalf("unknown token code=%d want 401", code)
	}
}

func TestAgentTokenHandler_NonLoopbackForbidden(t *testing.T) {
	svc := newTestService(t)
	if err := svc.AddPrimary(&fakeTTLSource{name: "hugr"}); err != nil {
		t.Fatalf("AddPrimary: %v", err)
	}
	sa, err := svc.NewStdioAuth(context.Background(), "hugr")
	if err != nil {
		t.Fatalf("NewStdioAuth: %v", err)
	}
	t.Cleanup(sa.RevokeFunc)

	req := httptest.NewRequest("POST", AgentTokenPath, strings.NewReader(`{"token":"`+sa.BootstrapToken+`"}`))
	req.RemoteAddr = "10.0.0.1:5000"
	rec := httptest.NewRecorder()
	svc.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-loopback code=%d want 403", rec.Code)
	}
}

func TestAgentTokenHandler_GetIs405(t *testing.T) {
	svc := newTestService(t)
	if err := svc.AddPrimary(&fakeTTLSource{name: "hugr"}); err != nil {
		t.Fatalf("AddPrimary: %v", err)
	}
	srv := agentTokenServer(t, svc)
	resp, err := srv.Client().Get(srv.URL + AgentTokenPath)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET code=%d want 405", resp.StatusCode)
	}
}

func TestAgentTokenHandler_BadJSONIs400(t *testing.T) {
	svc := newTestService(t)
	if err := svc.AddPrimary(&fakeTTLSource{name: "hugr"}); err != nil {
		t.Fatalf("AddPrimary: %v", err)
	}
	srv := agentTokenServer(t, svc)
	code, _ := postExchange(t, srv, "not json")
	if code != http.StatusBadRequest {
		t.Fatalf("bad JSON code=%d want 400", code)
	}
}

func TestAgentTokenHandler_SourceErrorIs502(t *testing.T) {
	svc := newTestService(t)
	src := &fakeTTLSource{name: "hugr", errs: []error{errors.New("source down")}}
	if err := svc.AddPrimary(src); err != nil {
		t.Fatalf("AddPrimary: %v", err)
	}
	sa, err := svc.NewStdioAuth(context.Background(), "hugr")
	if err != nil {
		t.Fatalf("NewStdioAuth: %v", err)
	}
	t.Cleanup(sa.RevokeFunc)

	srv := agentTokenServer(t, svc)
	code, _ := postExchange(t, srv, `{"token":"`+sa.BootstrapToken+`"}`)
	if code != http.StatusBadGateway {
		t.Fatalf("source-error code=%d want 502", code)
	}
}
