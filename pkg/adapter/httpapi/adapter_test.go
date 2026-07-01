package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hugr-lab/hugen/pkg/session/manager"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// stubHost satisfies adapter.Host (manager.AdapterHost). Only Logger() is
// reachable in the H1 code paths; the rest stay nil-embedded (unused).
type stubHost struct{ manager.AdapterHost }

func (stubHost) Logger() *slog.Logger { return quietLogger() }

func TestAdapter_Name(t *testing.T) {
	if got := New().Name(); got != "httpapi" {
		t.Fatalf("Name() = %q, want %q", got, "httpapi")
	}
}

func TestCard_Served(t *testing.T) {
	a := New(WithBaseURL("https://agent.example"), WithAgentIdentity("acme-bot", "does acme"))
	mux := http.NewServeMux()
	if err := a.mount(mux, true); err != nil {
		t.Fatalf("mount: %v", err)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cardPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: status %d, want 200", cardPath, rec.Code)
	}
	var card agentCard
	if err := json.Unmarshal(rec.Body.Bytes(), &card); err != nil {
		t.Fatalf("decode card: %v", err)
	}
	if card.Name != "acme-bot" || card.Description != "does acme" {
		t.Errorf("card identity = %q/%q, want acme-bot/does acme", card.Name, card.Description)
	}
	if card.Version == "" {
		t.Error("card.Version is empty")
	}
	if card.API.Protocol != "hugen/v1" || card.API.Transport != "sse+http" || card.API.BaseURL != "https://agent.example" {
		t.Errorf("card.API = %+v, want hugen/v1 sse+http https://agent.example", card.API)
	}
	if !card.Capabilities.Streaming || !card.Capabilities.Inquiry {
		t.Errorf("card.Capabilities = %+v, want streaming+inquiry", card.Capabilities)
	}
}

func TestHealth_Served(t *testing.T) {
	a := New()
	mux := http.NewServeMux()
	if err := a.mount(mux, true); err != nil {
		t.Fatalf("mount: %v", err)
	}
	for _, path := range []string{healthzPath, readyzPath} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
			t.Errorf("GET %s: %d %q, want 200 ok", path, rec.Code, rec.Body.String())
		}
	}
}

func TestRun_FailsClosedWithoutIssuer(t *testing.T) {
	// No issuer + no WithAllowOpen ⇒ Run refuses to serve (D4).
	a := New(WithLogger(quietLogger()), WithSharedMux(http.NewServeMux()))
	if err := a.Run(context.Background(), stubHost{}); err == nil {
		t.Fatal("Run served with no issuer and no WithAllowOpen — want a fail-closed error")
	}
}

func TestRun_SharedMount_AllowOpen(t *testing.T) {
	// allow-open + shared mux + an already-cancelled ctx: Run mounts, then
	// returns on ctx. The card is reachable on the shared mux afterwards.
	mux := http.NewServeMux()
	a := New(WithLogger(quietLogger()), WithSharedMux(mux), WithAllowOpen(true))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Run(ctx, stubHost{}); err != nil {
		t.Fatalf("Run(allow-open, cancelled ctx) = %v, want nil", err)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cardPath, nil))
	if rec.Code != http.StatusOK {
		t.Errorf("card after shared mount: status %d, want 200", rec.Code)
	}
}

func TestRun_WithIssuer_MountsWithoutAllowOpen(t *testing.T) {
	// An issuer configured ⇒ no allow-open needed to serve.
	mux := http.NewServeMux()
	a := New(WithLogger(quietLogger()), WithSharedMux(mux), WithIssuer("https://issuer.example"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Run(ctx, stubHost{}); err != nil {
		t.Fatalf("Run(issuer set) = %v, want nil", err)
	}
}
