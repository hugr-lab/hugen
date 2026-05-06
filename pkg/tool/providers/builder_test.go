package providers

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
	"github.com/hugr-lab/hugen/pkg/tool"

	"github.com/mark3labs/mcp-go/mcp"
	mcpsrv "github.com/mark3labs/mcp-go/server"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestBuilder_DispatchesUnknownType(t *testing.T) {
	b := NewBuilder(nil, nil, "", nil)
	_, err := b.Build(context.Background(), tool.Spec{Name: "x", Type: "webhook"})
	if err == nil {
		t.Fatal("Build should error for unknown type")
	}
	if !strings.Contains(err.Error(), "unknown type") {
		t.Errorf("err = %v", err)
	}
}

// Empty / "mcp" type both route to the mcp subpackage. We don't
// stand up a real MCP transport here — verifying the dispatch
// reaches mcp.New is enough; mcp's own tests pin the construction
// behaviour. Both shapes fail at the same validation point (stdio
// without Command), which is what we assert.
func TestBuilder_RoutesMCPDefault(t *testing.T) {
	b := NewBuilder(nil, nil, "", nil)
	for _, typ := range []string{"", "mcp", "MCP"} {
		_, err := b.Build(context.Background(), tool.Spec{
			Name:     "broken",
			Type:     typ,
			Lifetime: tool.LifetimePerSession,
		})
		if err == nil {
			t.Errorf("type=%q: expected error from mcp.New (stdio without Command)", typ)
		}
		// The error is wrapped from mcp.New — must NOT mention
		// "unknown type" (that's the default-case path).
		if err != nil && strings.Contains(err.Error(), "unknown type") {
			t.Errorf("type=%q: routed to default case instead of mcp: %v", typ, err)
		}
	}
}

// fakeProvidersView is a static implementation of
// config.ToolProvidersView for tests — no fs / hub backing.
type fakeProvidersView struct{ specs []config.ToolProviderSpec }

func (v *fakeProvidersView) Providers() []config.ToolProviderSpec { return v.specs }
func (v *fakeProvidersView) OnUpdate(func()) (cancel func())      { return func() {} }

// stubBearerSource is a sources.Source whose Token() always returns
// the static value passed in. Drives the HTTP-with-auth Init path
// without standing up the real sources/oidc machinery.
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

// TestInit_DegradesOnBadConfig — a misconfigured provider (empty
// endpoint) does not abort Init: it is logged and skipped, and Init
// still returns nil. Lives here (not in pkg/tool) because it
// exercises the production providers.Builder.
func TestInit_DegradesOnBadConfig(t *testing.T) {
	view := &fakeProvidersView{specs: []config.ToolProviderSpec{{
		Name:      "hugr-main",
		Type:      "mcp",
		Transport: "http",
		// Endpoint deliberately empty — buildSpec rejects it; Init
		// must skip + warn instead of aborting.
	}}}
	tm := tool.NewToolManager(nil, view, discardLogger(),
		tool.WithBuilder(NewBuilder(nil, nil, "", discardLogger())))
	t.Cleanup(func() { _ = tm.Close() })
	if err := tm.Init(context.Background()); err != nil {
		t.Fatalf("Init aborted on bad config: %v", err)
	}
	if got := tm.Providers(); len(got) != 0 {
		t.Fatalf("expected no providers registered, got %v", got)
	}
}

// TestInit_DegradesOnConnectFailure — a syntactically valid HTTP
// entry pointed at an unreachable endpoint is also skipped + warned
// rather than aborting boot.
func TestInit_DegradesOnConnectFailure(t *testing.T) {
	view := &fakeProvidersView{specs: []config.ToolProviderSpec{{
		Name:      "hugr-main",
		Type:      "mcp",
		Transport: "http",
		// 127.0.0.1:1 — no listener; connect will fail.
		Endpoint: "http://127.0.0.1:1/mcp",
	}}}
	tm := tool.NewToolManager(nil, view, discardLogger(),
		tool.WithBuilder(NewBuilder(nil, nil, "", discardLogger())))
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

// TestInit_HTTPLive verifies the integration: providers.Builder +
// providers/mcp.New successfully connect to a live HTTP MCP server
// with bearer-injecting auth and the provider lands in the manager.
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
	tm := tool.NewToolManager(nil, view, discardLogger(),
		tool.WithBuilder(NewBuilder(svc, nil, "", discardLogger())))
	t.Cleanup(func() { _ = tm.Close() })

	if err := tm.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if atomic.LoadInt64(oks) == 0 {
		t.Fatal("expected at least one bearer-OK request to MCP server during init")
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
