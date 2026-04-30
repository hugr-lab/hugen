package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpsrv "github.com/mark3labs/mcp-go/server"
)

// newAuthedHTTPMCP spins up an in-process streamable-http MCP server
// that requires Authorization: Bearer <wantToken>. It returns the
// httptest server, a counter of requests received with the right
// bearer, and a counter of requests that started rejected (used to
// fail-then-succeed tests).
func newAuthedHTTPMCP(t *testing.T, wantToken string, rejectFirst bool) (*httptest.Server, *int64, *int64) {
	t.Helper()

	mcpServer := mcpsrv.NewMCPServer("hugr-main-test", "0.1.0",
		mcpsrv.WithToolCapabilities(true),
	)
	mcpServer.AddTool(mcp.NewTool("echo",
		mcp.WithDescription("returns its input"),
		mcp.WithString("text", mcp.Required()),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		text, _ := args["text"].(string)
		return mcp.NewToolResultText("got: " + text), nil
	})

	streamable := mcpsrv.NewStreamableHTTPServer(mcpServer)

	var ok, rejected int64
	var rejectedOnce atomic.Bool
	if !rejectFirst {
		rejectedOnce.Store(true) // already past the rejection window
	}

	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !rejectedOnce.Load() {
			rejectedOnce.Store(true)
			atomic.AddInt64(&rejected, 1)
			http.Error(w, "transient 401", http.StatusUnauthorized)
			return
		}
		if auth != "Bearer "+wantToken {
			atomic.AddInt64(&rejected, 1)
			http.Error(w, "bad bearer", http.StatusUnauthorized)
			return
		}
		atomic.AddInt64(&ok, 1)
		streamable.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(wrapped)
	t.Cleanup(srv.Close)
	return srv, &ok, &rejected
}

// fixedTokenRoundTripper is a tiny http.RoundTripper that injects a
// static Bearer token. Used in tests to exercise the
// MCPProviderSpec.RoundTripper field without pulling in pkg/auth.
type fixedTokenRoundTripper struct {
	token string
	base  http.RoundTripper
}

func (rt *fixedTokenRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+rt.token)
	base := rt.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(r)
}

func TestMCPProvider_HTTP_BearerInjection(t *testing.T) {
	srv, ok, rejected := newAuthedHTTPMCP(t, "ok-token", false)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	prov, err := NewMCPProvider(ctx, MCPProviderSpec{
		Name:         "hugr-main",
		Transport:    TransportStreamableHTTP,
		Endpoint:     srv.URL,
		RoundTripper: &fixedTokenRoundTripper{token: "ok-token"},
		Lifetime:     LifetimePerAgent,
		PermObject:   "hugen:tool:hugr-main",
	}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewMCPProvider: %v", err)
	}
	defer prov.Close()

	tools, err := prov.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "hugr-main:echo" {
		t.Fatalf("unexpected tools: %+v", tools)
	}

	args, _ := json.Marshal(map[string]any{"text": "hi"})
	res, err := prov.Call(ctx, "echo", args)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(res, &payload); err != nil {
		t.Fatalf("unmarshal result: %v (raw=%s)", err, string(res))
	}
	if payload.Text != "got: hi" {
		t.Fatalf("unexpected text %q", payload.Text)
	}
	if atomic.LoadInt64(rejected) != 0 {
		t.Fatalf("expected 0 rejected, got %d", atomic.LoadInt64(rejected))
	}
	if atomic.LoadInt64(ok) == 0 {
		t.Fatalf("expected at least one OK request")
	}
}

func TestMCPProvider_HTTP_MissingEndpoint(t *testing.T) {
	_, err := NewMCPProvider(context.Background(), MCPProviderSpec{
		Name:      "broken",
		Transport: TransportStreamableHTTP,
	}, slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("expected error on missing endpoint")
	}
}

func TestMCPProvider_HTTP_BadBearerFails(t *testing.T) {
	srv, _, rejected := newAuthedHTTPMCP(t, "ok-token", false)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := NewMCPProvider(ctx, MCPProviderSpec{
		Name:         "hugr-main",
		Transport:    TransportStreamableHTTP,
		Endpoint:     srv.URL,
		RoundTripper: &fixedTokenRoundTripper{token: "wrong"},
	}, slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("expected initialize to fail under bad bearer")
	}
	if atomic.LoadInt64(rejected) == 0 {
		t.Fatalf("expected at least one rejection")
	}
}

func TestMCPProvider_HTTP_StaticHeaders(t *testing.T) {
	var seen atomic.Pointer[string]
	mcpServer := mcpsrv.NewMCPServer("h", "0",
		mcpsrv.WithToolCapabilities(true),
	)
	mcpServer.AddTool(mcp.NewTool("ping"), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("pong"), nil
	})
	streamable := mcpsrv.NewStreamableHTTPServer(mcpServer)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := r.Header.Get("X-Api-Key")
		if v != "" {
			seen.Store(&v)
		}
		streamable.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	prov, err := NewMCPProvider(ctx, MCPProviderSpec{
		Name:      "h",
		Transport: TransportStreamableHTTP,
		Endpoint:  srv.URL,
		Headers:   map[string]string{"X-Api-Key": "secret-123"},
	}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewMCPProvider: %v", err)
	}
	defer prov.Close()

	if _, err := prov.List(ctx); err != nil {
		t.Fatalf("List: %v", err)
	}
	got := seen.Load()
	if got == nil || *got != "secret-123" {
		if got == nil {
			t.Fatal("expected X-Api-Key header on outbound request, got none")
		}
		t.Fatalf("unexpected header value: %q", *got)
	}
}

// TestMCPProvider_HTTP_DynamicTokenViaRoundTripper proves the
// RoundTripper is consulted on every request — exactly what the
// hugr-main wiring needs (per-call token from auth.Source.Token).
func TestMCPProvider_HTTP_DynamicTokenViaRoundTripper(t *testing.T) {
	srv, _, _ := newAuthedHTTPMCP(t, "ok-token", false)

	var calls int64
	rt := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt64(&calls, 1)
		r := req.Clone(req.Context())
		r.Header.Set("Authorization", "Bearer ok-token")
		return http.DefaultTransport.RoundTrip(r)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	prov, err := NewMCPProvider(ctx, MCPProviderSpec{
		Name:         "hugr-main",
		Transport:    TransportStreamableHTTP,
		Endpoint:     srv.URL,
		RoundTripper: rt,
	}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewMCPProvider: %v", err)
	}
	defer prov.Close()

	if _, err := prov.List(ctx); err != nil {
		t.Fatalf("List: %v", err)
	}
	args, _ := json.Marshal(map[string]any{"text": "x"})
	if _, err := prov.Call(ctx, "echo", args); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got := atomic.LoadInt64(&calls); got < 2 {
		t.Fatalf("expected RoundTripper called at least twice (init+list+call), got %d", got)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// silence unused-import warnings if a test path is removed.
var _ = errors.New
var _ = fmt.Sprintf
