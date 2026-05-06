package mcp

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/hugr-lab/hugen/pkg/auth/sources"
	"github.com/hugr-lab/hugen/pkg/config"
)

// stubBearerSource is a tiny sources.Source whose Token() always
// returns the static value passed to the test. Used to verify the
// HTTP path of buildSpec wires a RoundTripper.
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
	svc := auth.NewService(nil, http.NewServeMux(), "", 0, false)
	if err := svc.Add(src); err != nil {
		t.Fatalf("auth.Add: %v", err)
	}
	return svc
}

func TestBuildSpec_HugrMain_HTTPWithAuth(t *testing.T) {
	svc := newAuthSvcWith(t, &stubBearerSource{name: "hugr", token: "tk"})

	got, _, err := buildSpec(config.ToolProviderSpec{
		Name:      "hugr-main",
		Type:      "mcp",
		Transport: "http",
		Endpoint:  "https://hugr.example.com/mcp",
		Auth:      "hugr",
	}, svc, "")
	if err != nil {
		t.Fatalf("buildSpec: %v", err)
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

func TestBuildSpec_MissingEndpoint(t *testing.T) {
	_, _, err := buildSpec(config.ToolProviderSpec{
		Name:      "hugr-main",
		Type:      "mcp",
		Transport: "http",
	}, nil, "")
	if err == nil || !strings.Contains(err.Error(), "missing endpoint") {
		t.Fatalf("expected missing-endpoint error, got %v", err)
	}
}

func TestBuildSpec_AuthWithoutService(t *testing.T) {
	_, _, err := buildSpec(config.ToolProviderSpec{
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

func TestBuildSpec_AuthSourceMissing(t *testing.T) {
	svc := auth.NewService(nil, http.NewServeMux(), "", 0, false)
	_, _, err := buildSpec(config.ToolProviderSpec{
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

func TestBuildSpec_GenericHTTP(t *testing.T) {
	got, _, err := buildSpec(config.ToolProviderSpec{
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

func TestBuildSpec_StdioMissingCommand(t *testing.T) {
	_, _, err := buildSpec(config.ToolProviderSpec{
		Name: "x",
		Type: "mcp",
	}, nil, "")
	if err == nil || !strings.Contains(err.Error(), "missing command") {
		t.Fatalf("expected missing-command error, got %v", err)
	}
}
