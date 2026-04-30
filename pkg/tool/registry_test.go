package tool

import (
	"context"
	"errors"
	"time"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hugr-lab/hugen/pkg/config"

	"github.com/mark3labs/mcp-go/mcp"
	mcpsrv "github.com/mark3labs/mcp-go/server"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestBuildMCPProviderSpec_HugrMain_HTTPWithAuth(t *testing.T) {
	called := false
	rt := AuthResolverFunc(func(name string) (http.RoundTripper, error) {
		if name != "hugr" {
			t.Fatalf("unexpected auth name %q", name)
		}
		called = true
		return roundTripperStub{}, nil
	})

	got, err := BuildMCPProviderSpec(config.ToolProviderSpec{
		Name:      "hugr-main",
		Type:      "mcp",
		Transport: "http",
		Endpoint:  "https://hugr.example.com/mcp",
		Auth:      "hugr",
	}, rt)
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
	if !called {
		t.Error("AuthResolver never invoked")
	}
}

func TestBuildMCPProviderSpec_MissingEndpoint(t *testing.T) {
	_, err := BuildMCPProviderSpec(config.ToolProviderSpec{
		Name:      "hugr-main",
		Type:      "mcp",
		Transport: "http",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "missing endpoint") {
		t.Fatalf("expected missing-endpoint error, got %v", err)
	}
}

func TestBuildMCPProviderSpec_AuthWithoutResolver(t *testing.T) {
	_, err := BuildMCPProviderSpec(config.ToolProviderSpec{
		Name:      "hugr-main",
		Type:      "mcp",
		Transport: "http",
		Endpoint:  "http://x",
		Auth:      "hugr",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "no resolver") {
		t.Fatalf("expected no-resolver error, got %v", err)
	}
}

func TestBuildMCPProviderSpec_ResolverError(t *testing.T) {
	want := errors.New("missing")
	rt := AuthResolverFunc(func(name string) (http.RoundTripper, error) { return nil, want })
	_, err := BuildMCPProviderSpec(config.ToolProviderSpec{
		Name:      "hugr-main",
		Type:      "mcp",
		Transport: "http",
		Endpoint:  "http://x",
		Auth:      "hugr",
	}, rt)
	if err == nil || !errors.Is(err, want) {
		t.Fatalf("err chain missing resolver error: %v", err)
	}
}

func TestBuildMCPProviderSpec_GenericHTTP(t *testing.T) {
	got, err := BuildMCPProviderSpec(config.ToolProviderSpec{
		Name:      "weather",
		Type:      "mcp",
		Transport: "http",
		Endpoint:  "http://w/mcp",
	}, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.Endpoint != "http://w/mcp" {
		t.Fatalf("Endpoint = %q", got.Endpoint)
	}
}

func TestBuildMCPProviderSpec_StdioMissingCommand(t *testing.T) {
	_, err := BuildMCPProviderSpec(config.ToolProviderSpec{
		Name: "x",
		Type: "mcp",
	}, nil)
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
	tm := NewToolManager(nil, nil, nil, nil, discardLogger())
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
	tm := NewToolManager(nil, nil, view, nil, discardLogger())
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
	tm := NewToolManager(nil, nil, view, nil, discardLogger())
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

	rt := AuthResolverFunc(func(name string) (http.RoundTripper, error) {
		return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			r := req.Clone(req.Context())
			r.Header.Set("Authorization", "Bearer ok-token")
			return http.DefaultTransport.RoundTrip(r)
		}), nil
	})

	view := &fakeProvidersView{specs: []config.ToolProviderSpec{{
		Name:      "hugr-main",
		Type:      "mcp",
		Transport: "http",
		Endpoint:  srv.URL,
		Auth:      "hugr",
	}}}
	tm := NewToolManager(nil, nil, view, rt, discardLogger())
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
	tm := NewToolManager(nil, nil, view, nil, discardLogger())
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

type roundTripperStub struct{}

func (roundTripperStub) RoundTrip(*http.Request) (*http.Response, error) { panic("never called") }
