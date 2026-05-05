package tool

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/hugr-lab/hugen/pkg/auth/sources"
	"github.com/hugr-lab/hugen/pkg/config"

	"github.com/mark3labs/mcp-go/mcp"
	mcpsrv "github.com/mark3labs/mcp-go/server"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// stubBearerSource implements sources.Source with a static token —
// auth.Service.TokenStore returns it, auth.Transport wraps it as a
// bearer header on outbound requests.
type stubBearerSource struct {
	name  string
	token string
}

func (s *stubBearerSource) Name() string                          { return s.name }
func (s *stubBearerSource) Token(context.Context) (string, error) { return s.token, nil }
func (s *stubBearerSource) Login(context.Context) error           { return nil }
func (s *stubBearerSource) OwnsState(state string) bool           { return sources.StateOwnedBy(s.name, state) }
func (s *stubBearerSource) HandleCallback(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "no", http.StatusBadRequest)
}

func newAuthSvcWith(t *testing.T, src sources.Source) *auth.Service {
	t.Helper()
	svc := auth.NewService(discardLogger(), http.NewServeMux(), "", 0, false)
	if err := svc.Add(src); err != nil {
		t.Fatalf("auth.Add: %v", err)
	}
	return svc
}

func TestBuildMCPProviderSpec_HugrMain_HTTPWithAuth(t *testing.T) {
	svc := newAuthSvcWith(t, &stubBearerSource{name: "hugr", token: "tk"})

	got, _, err := BuildMCPProviderSpec(config.ToolProviderSpec{
		Name:      "hugr-main",
		Type:      "mcp",
		Transport: "http",
		Endpoint:  "https://hugr.example.com/mcp",
		Auth:      "hugr",
	}, svc, "")
	if err != nil {
		t.Fatalf("BuildMCPProviderSpec: %v", err)
	}
	if got.Transport != TransportStreamableHTTP {
		t.Errorf("Transport = %q want http", got.Transport)
	}
	if got.Endpoint != "https://hugr.example.com/mcp" {
		t.Errorf("Endpoint = %q", got.Endpoint)
	}
	if got.RoundTripper == nil {
		t.Error("RoundTripper not wired")
	}
	if got.PermObject != "hugen:tool:hugr-main" {
		t.Errorf("PermObject = %q", got.PermObject)
	}
}

func TestBuildMCPProviderSpec_MissingEndpoint(t *testing.T) {
	_, _, err := BuildMCPProviderSpec(config.ToolProviderSpec{
		Name:      "hugr-main",
		Type:      "mcp",
		Transport: "http",
	}, nil, "")
	if err == nil || !strings.Contains(err.Error(), "missing endpoint") {
		t.Fatalf("expected missing-endpoint error, got %v", err)
	}
}

func TestBuildMCPProviderSpec_AuthWithoutService(t *testing.T) {
	_, _, err := BuildMCPProviderSpec(config.ToolProviderSpec{
		Name:      "hugr-main",
		Type:      "mcp",
		Transport: "http",
		Endpoint:  "http://x",
		Auth:      "hugr",
	}, nil, "")
	if err == nil || !strings.Contains(err.Error(), "auth.Service") {
		t.Fatalf("expected no-service error, got %v", err)
	}
}

func TestBuildMCPProviderSpec_AuthSourceMissing(t *testing.T) {
	svc := auth.NewService(discardLogger(), http.NewServeMux(), "", 0, false)
	_, _, err := BuildMCPProviderSpec(config.ToolProviderSpec{
		Name:      "hugr-main",
		Type:      "mcp",
		Transport: "http",
		Endpoint:  "http://x",
		Auth:      "hugr",
	}, svc, "")
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("expected source-missing error, got %v", err)
	}
}

func TestBuildMCPProviderSpec_GenericHTTP(t *testing.T) {
	got, _, err := BuildMCPProviderSpec(config.ToolProviderSpec{
		Name:      "weather",
		Type:      "mcp",
		Transport: "http",
		Endpoint:  "http://w/mcp",
	}, nil, "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.Endpoint != "http://w/mcp" {
		t.Fatalf("Endpoint = %q", got.Endpoint)
	}
}

func TestBuildMCPProviderSpec_StdioMissingCommand(t *testing.T) {
	_, _, err := BuildMCPProviderSpec(config.ToolProviderSpec{
		Name: "x",
		Type: "mcp",
	}, nil, "")
	if err == nil || !strings.Contains(err.Error(), "missing command") {
		t.Fatalf("expected missing-command error, got %v", err)
	}
}

// fakeProvidersView is a static implementation of
// config.ToolProvidersView for tests — no fs / hub backing.
type fakeProvidersView struct{ specs []config.ToolProviderSpec }

func (v *fakeProvidersView) Providers() []config.ToolProviderSpec { return v.specs }
func (v *fakeProvidersView) OnUpdate(func()) (cancel func())      { return func() {} }

func TestInit_NilView(t *testing.T) {
	tm := NewToolManager(nil, nil, nil, discardLogger())
	t.Cleanup(func() { _ = tm.Close() })
	if err := tm.Init(context.Background()); err != nil {
		t.Fatalf("expected no-op, got %v", err)
	}
}

// TestInit_DegradesOnBadConfig verifies that a misconfigured
// provider (empty endpoint, etc.) does not abort Init — it is
// logged and skipped, and Init still returns nil.
func TestInit_DegradesOnBadConfig(t *testing.T) {
	view := &fakeProvidersView{specs: []config.ToolProviderSpec{{
		Name:      "hugr-main",
		Type:      "mcp",
		Transport: "http",
		// Endpoint deliberately empty — BuildMCPProviderSpec rejects
		// it; Init must skip + warn instead of aborting.
	}}}
	tm := NewToolManager(nil, view, nil, discardLogger())
	t.Cleanup(func() { _ = tm.Close() })
	if err := tm.Init(context.Background()); err != nil {
		t.Fatalf("Init aborted on bad config: %v", err)
	}
	if got := tm.Providers(); len(got) != 0 {
		t.Fatalf("expected no providers registered, got %v", got)
	}
}

// TestInit_DegradesOnConnectFailure verifies that a syntactically
// valid HTTP entry pointed at an unreachable endpoint is also
// skipped + warned rather than aborting boot.
func TestInit_DegradesOnConnectFailure(t *testing.T) {
	view := &fakeProvidersView{specs: []config.ToolProviderSpec{{
		Name:      "hugr-main",
		Type:      "mcp",
		Transport: "http",
		// 127.0.0.1:1 — no listener; connect will fail.
		Endpoint: "http://127.0.0.1:1/mcp",
	}}}
	tm := NewToolManager(nil, view, nil, discardLogger())
	t.Cleanup(func() { _ = tm.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tm.Init(ctx); err != nil {
		t.Fatalf("Init aborted on unreachable endpoint: %v", err)
	}
	if got := tm.Providers(); len(got) != 0 {
		t.Fatalf("expected no providers registered, got %v", got)
	}
}

func TestInit_HTTPLive(t *testing.T) {
	srv, oks := startAuthedTestMCP(t, "ok-token")

	svc := newAuthSvcWith(t, &stubBearerSource{name: "hugr", token: "ok-token"})

	view := &fakeProvidersView{specs: []config.ToolProviderSpec{{
		Name:      "hugr-main",
		Type:      "mcp",
		Transport: "http",
		Endpoint:  srv.URL,
		Auth:      "hugr",
	}}}
	tm := NewToolManager(nil, view, svc, discardLogger())
	t.Cleanup(func() { _ = tm.Close() })

	if err := tm.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if atomic.LoadInt64(oks) == 0 {
		t.Fatal("expected at least one bearer-OK request to MCP server during init")
	}
}

func TestInit_SkipsPerSession(t *testing.T) {
	view := &fakeProvidersView{specs: []config.ToolProviderSpec{{
		Name:     "bash-mcp",
		Type:     "mcp",
		Command:  "bash-mcp",
		Lifetime: "per_session",
	}}}
	tm := NewToolManager(nil, view, nil, discardLogger())
	t.Cleanup(func() { _ = tm.Close() })
	if err := tm.Init(context.Background()); err != nil {
		t.Fatalf("expected per_session entries skipped, got %v", err)
	}
}

// startAuthedTestMCP spins up an in-process streamable-http MCP
// server fronted by a bearer-checking middleware. Returns the
// httptest URL and a counter of bearer-OK requests.
func startAuthedTestMCP(t *testing.T, wantBearer string) (*httptest.Server, *int64) {
	t.Helper()
	mcpServer := mcpsrv.NewMCPServer("hugr-main-test", "0.1.0", mcpsrv.WithToolCapabilities(true))
	mcpServer.AddTool(mcp.NewTool("hello"), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("hi"), nil
	})
	streamable := mcpsrv.NewStreamableHTTPServer(mcpServer)
	var oks int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+wantBearer {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		atomic.AddInt64(&oks, 1)
		streamable.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &oks
}
