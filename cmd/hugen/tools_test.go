package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/hugr-lab/hugen/pkg/auth/sources"
)

// stubAuthSource matches the pattern used in pkg/auth/service_test.go.
// Only the Source bits exercised by the registry are non-trivial.
type stubAuthSource struct {
	name  string
	token string
}

func (s *stubAuthSource) Name() string                                          { return s.name }
func (s *stubAuthSource) Token(context.Context) (string, error)                 { return s.token, nil }
func (s *stubAuthSource) Login(context.Context) error                           { return nil }
func (s *stubAuthSource) OwnsState(state string) bool                           { return sources.StateOwnedBy(s.name, state) }
func (s *stubAuthSource) HandleCallback(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", 501) }

func newDiscardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestAuthResolverFor_RegisteredSource(t *testing.T) {
	svc := auth.NewService(newDiscardLogger(), http.NewServeMux(), "")
	if err := svc.Add(&stubAuthSource{name: "hugr", token: "tk"}); err != nil {
		t.Fatalf("auth.Add: %v", err)
	}
	rt, err := authResolverFor(svc).RoundTripper("hugr")
	if err != nil {
		t.Fatalf("RoundTripper: %v", err)
	}
	if rt == nil {
		t.Fatal("nil RoundTripper")
	}
}

func TestAuthResolverFor_UnknownSource(t *testing.T) {
	svc := auth.NewService(newDiscardLogger(), http.NewServeMux(), "")
	_, err := authResolverFor(svc).RoundTripper("missing")
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("expected unknown-source error, got %v", err)
	}
}

func TestAuthResolverFor_NilService(t *testing.T) {
	_, err := authResolverFor(nil).RoundTripper("hugr")
	if err == nil || !strings.Contains(err.Error(), "auth service") {
		t.Fatalf("expected nil-service error, got %v", err)
	}
}
