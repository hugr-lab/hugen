package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/tool"
	mcpcli "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func newInProcessProvider(t *testing.T, srv *server.MCPServer) *Provider {
	t.Helper()
	cli, err := mcpcli.NewInProcessClient(srv)
	if err != nil {
		t.Fatalf("NewInProcessClient: %v", err)
	}
	if err := cli.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	p, err := newWithClient(context.Background(), Spec{
		Name:       "stub",
		PermObject: "hugen:tool:stub",
		Lifetime:   tool.LifetimePerAgent,
	}, cli, nil)
	if err != nil {
		t.Fatalf("newWithClient: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func TestMCPProvider_ListAndCall_RoundTrip(t *testing.T) {
	srv := server.NewMCPServer("stub", "0.0.1", server.WithToolCapabilities(true))
	srv.AddTool(
		mcp.NewTool("echo",
			mcp.WithDescription("Echoes its input."),
			mcp.WithString("text", mcp.Required()),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			text, _ := req.RequireString("text")
			return mcp.NewToolResultText(text), nil
		},
	)

	p := newInProcessProvider(t, srv)
	tools, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "stub:echo" {
		t.Errorf("List = %+v", tools)
	}
	if tools[0].Provider != "stub" || tools[0].PermissionObject != "hugen:tool:stub" {
		t.Errorf("provider/perm = %+v", tools[0])
	}

	out, err := p.Call(context.Background(), "echo", json.RawMessage(`{"text":"hello"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(string(out), `"hello"`) {
		t.Errorf("Call result = %s", out)
	}
}

func TestMCPProvider_Call_PassesEmptyArgsAsObject(t *testing.T) {
	srv := server.NewMCPServer("stub", "0.0.1", server.WithToolCapabilities(true))
	got := ""
	srv.AddTool(
		mcp.NewTool("ping"),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			got = "called"
			return mcp.NewToolResultText("pong"), nil
		},
	)
	p := newInProcessProvider(t, srv)
	if _, err := p.Call(context.Background(), "ping", nil); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got != "called" {
		t.Errorf("handler not invoked")
	}
}

func TestMCPProvider_Subscribe_FanOut(t *testing.T) {
	// In-process transport doesn't register a session unless
	// sampling/elicitation handlers are wired, so the server's
	// SendNotificationToAllClients won't reach the client. Real
	// list_changed coverage lives in T037 (bash-mcp e2e); here we
	// exercise the subscriber fan-out path directly.
	srv := server.NewMCPServer("stub", "0.0.1", server.WithToolCapabilities(true))
	p := newInProcessProvider(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := p.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	p.emit(tool.ProviderEvent{Kind: tool.ProviderToolsChanged})

	select {
	case ev := <-ch:
		if ev.Kind != tool.ProviderToolsChanged {
			t.Errorf("event kind = %v, want ProviderToolsChanged", ev.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("ProviderToolsChanged not delivered")
	}
}

func TestMCPProvider_Close_Idempotent(t *testing.T) {
	srv := server.NewMCPServer("stub", "0.0.1", server.WithToolCapabilities(true))
	p := newInProcessProvider(t, srv)
	if err := p.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if _, err := p.List(context.Background()); !errors.Is(err, tool.ErrProviderRemoved) {
		t.Errorf("List after Close = %v, want ErrProviderRemoved", err)
	}
}

func TestMarshalCallResult_TextOnly(t *testing.T) {
	res := &mcp.CallToolResult{Content: []mcp.Content{
		mcp.TextContent{Type: "text", Text: "hello"},
	}}
	out, err := marshalCallResult(res)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"hello"`) {
		t.Errorf("out = %s", out)
	}
}

func TestMarshalCallResult_StructuredWins(t *testing.T) {
	res := &mcp.CallToolResult{
		Content:           []mcp.Content{mcp.TextContent{Type: "text", Text: "ignored"}},
		StructuredContent: map[string]any{"k": "v"},
	}
	out, err := marshalCallResult(res)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["k"] != "v" {
		t.Errorf("got = %v", got)
	}
}

func TestMarshalCallResult_IsErrorPropagates(t *testing.T) {
	res := &mcp.CallToolResult{
		Content: []mcp.Content{mcp.TextContent{Type: "text", Text: "boom"}},
		IsError: true,
	}
	out, err := marshalCallResult(res)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"is_error":true`) {
		t.Errorf("out = %s", out)
	}
}

func TestEnvSlice(t *testing.T) {
	// Pin a unique parent var so we can spot inheritance without
	// caring about the rest of the parent environment.
	const sentinel = "BASH_MCP_TEST_SENTINEL_VAR"
	t.Setenv(sentinel, "parent-value")

	out := envSlice(nil)
	if !envContains(out, sentinel+"=parent-value") {
		t.Errorf("nil override: missing inherited %s in %v", sentinel, out)
	}

	out = envSlice(map[string]string{"K": "v"})
	if !envContains(out, "K=v") {
		t.Errorf("override: missing K=v")
	}
	if !envContains(out, sentinel+"=parent-value") {
		t.Errorf("override should still inherit %s", sentinel)
	}

	// Override of an existing parent var replaces (not duplicates).
	out = envSlice(map[string]string{sentinel: "overridden"})
	if !envContains(out, sentinel+"=overridden") || envContains(out, sentinel+"=parent-value") {
		t.Errorf("override of inherited var failed: %v", out)
	}
}

func envContains(env []string, want string) bool {
	for _, kv := range env {
		if kv == want {
			return true
		}
	}
	return false
}

// TestProvider_TryReconnectFailurePropagates: a failed
// connect() during TryReconnect must surface the error verbatim.
// recovery.Wrap drives the retry loop; the provider does not
// classify or persist failure state.
func TestProvider_TryReconnectFailurePropagates(t *testing.T) {
	p := &Provider{
		spec: Spec{
			Name:      "broken",
			Transport: "unsupported", // makes connect() fail deterministically
		},
		log: slog.New(slog.DiscardHandler),
	}
	if err := p.TryReconnect(context.Background()); err == nil {
		t.Fatal("TryReconnect should return the connect error verbatim")
	}
}

// TestProvider_TryReconnectAfterCloseRefused: TryReconnect on a
// closed provider returns tool.ErrProviderRemoved without
// attempting a reconnect — Close is final.
func TestProvider_TryReconnectAfterCloseRefused(t *testing.T) {
	p := &Provider{
		spec: Spec{Name: "p", Transport: TransportStdio},
		log:  slog.New(slog.DiscardHandler),
	}
	p.closed = true
	if err := p.TryReconnect(context.Background()); !errors.Is(err, tool.ErrProviderRemoved) {
		t.Errorf("TryReconnect after Close = %v, want ErrProviderRemoved", err)
	}
}
