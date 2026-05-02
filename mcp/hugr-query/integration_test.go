package main_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// staticPrimary is a sources.Source + ttlAware impl that returns a
// constant JWT — drives auth.Service through its NewStdioAuth path
// without needing a real Hugr.
type staticPrimary struct{ jwt string }

func (s *staticPrimary) Name() string { return "hugr" }
func (s *staticPrimary) Token(ctx context.Context) (string, error) {
	tok, _, err := s.TokenWithTTL(ctx)
	return tok, err
}
func (s *staticPrimary) TokenWithTTL(_ context.Context) (string, int, error) { return s.jwt, 3600, nil }
func (s *staticPrimary) Login(context.Context) error                         { return nil }
func (s *staticPrimary) OwnsState(string) bool                               { return false }
func (s *staticPrimary) HandleCallback(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "no", http.StatusBadRequest)
}

// buildBinary compiles mcp/hugr-query into a temp file. Tests run
// from the package directory; we walk to repo root via "../..".
func buildBinary(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "hugr-query")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	repoRoot := filepath.Join("..", "..")
	cmd := exec.Command("go", "build", "-tags=duckdb_arrow", "-o", bin, "./mcp/hugr-query")
	cmd.Dir = repoRoot
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go build hugr-query: %v", err)
	}
	return bin
}

// startAgentTokenServer wires up an auth.Service backed by a static
// primary, mounts its mux on an httptest server, and returns the URL
// + the service so the test can mint StdioAuth bindings.
func startAgentTokenServer(t *testing.T, jwt string) (*httptest.Server, *auth.Service) {
	t.Helper()
	mux := http.NewServeMux()
	logger := slog.New(slog.DiscardHandler)
	svc := auth.NewService(logger, mux, "", 0, false)
	if err := svc.AddPrimary(&staticPrimary{jwt: jwt}); err != nil {
		t.Fatalf("AddPrimary: %v", err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, svc
}

// startHugrStub returns an httptest server that records the
// bearer tokens it received and replies with a static error
// response (we are not exercising query execution here, only the
// auth round-trip).
type hugrStub struct {
	srv         *httptest.Server
	authHeaders atomic.Pointer[[]string]
}

func (s *hugrStub) Authorizations() []string {
	p := s.authHeaders.Load()
	if p == nil {
		return nil
	}
	cp := make([]string, len(*p))
	copy(cp, *p)
	return cp
}

func startHugrStub(t *testing.T) *hugrStub {
	t.Helper()
	stub := &hugrStub{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		cur := stub.authHeaders.Load()
		var next []string
		if cur != nil {
			next = append(next, *cur...)
		}
		next = append(next, auth)
		stub.authHeaders.Store(&next)
		// Reply with a JSON error envelope. The hugr client expects
		// multipart/* so this triggers a "expected multipart" error
		// path, which is fine — the goal is to capture the bearer
		// header, not parse a successful response.
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintln(w, `{"errors":[{"message":"stub: not a real Hugr"}]}`)
	})
	stub.srv = httptest.NewServer(mux)
	t.Cleanup(stub.srv.Close)
	return stub
}

// TestHugrQuery_BootstrapAuthFlow verifies the end-to-end auth
// plumbing: the runtime mints a bootstrap secret, hands it to
// hugr-query via env, hugr-query exchanges it through
// /api/auth/agent-token, and uses the resulting JWT against the
// upstream Hugr endpoint.
func TestHugrQuery_BootstrapAuthFlow(t *testing.T) {
	const jwt = "real-hugr-jwt-token"
	tokenSrv, svc := startAgentTokenServer(t, jwt)
	hugrSrv := startHugrStub(t)

	sa, err := svc.NewStdioAuth(context.Background(), "hugr")
	if err != nil {
		t.Fatalf("NewStdioAuth: %v", err)
	}
	t.Cleanup(sa.RevokeFunc)
	bootstrap := sa.BootstrapToken

	bin := buildBinary(t)
	wsRoot := t.TempDir()
	sessionID := "ses-int-1"
	if err := os.MkdirAll(filepath.Join(wsRoot, sessionID, "data"), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	spec := tool.MCPProviderSpec{
		Name:      "hugr-query",
		Command:   bin,
		Lifetime:  tool.LifetimePerAgent,
		Transport: tool.TransportStdio,
		Env: map[string]string{
			"HUGR_URL":          hugrSrv.srv.URL,
			"HUGR_TOKEN_URL":    tokenSrv.URL + "/api/auth/agent-token",
			"HUGR_ACCESS_TOKEN": bootstrap,
			"WORKSPACES_ROOT":   wsRoot,
		},
	}
	prov, err := tool.NewMCPProvider(ctx, spec, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewMCPProvider: %v", err)
	}
	t.Cleanup(func() { _ = prov.Close() })

	// 1. Tool catalogue is exposed.
	tools, err := prov.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	for _, want := range []string{"hugr-query:query", "hugr-query:query_jq"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing tool %q in %v", want, names)
		}
	}

	// 2. Call hugr.Query — auth flow runs end-to-end. The Hugr
	// stub will reply with a non-multipart error that hugr-query
	// reports back as `tool_error{code:hugr_error}`. What we care
	// about: did the bearer header on the upstream call end up
	// being the real JWT (not the bootstrap secret)?
	args := json.RawMessage(`{"graphql":"{ ping }"}`)
	cctx := perm.WithSession(ctx, perm.SessionContext{SessionID: sessionID})
	resp, err := prov.Call(cctx, "query", args)
	// We expect SOME error envelope back (stub Hugr is not
	// multipart). The error doesn't need to be from MCP transport
	// — a tool-level error is JSON-encoded into the response.
	if err != nil && !strings.Contains(err.Error(), "tool: call") {
		// connection-level errors are unexpected
		var nope error = errors.New("unexpected MCP transport error")
		t.Fatalf("Call: %v: %v", nope, err)
	}
	_ = resp

	// 3. Verify auth header sequence: Hugr stub MUST have seen
	// the real JWT (not the bootstrap secret). The bootstrap is
	// strictly a token-exchange credential — leaking it onto Hugr
	// would defeat the whole bootstrap design.
	headers := hugrSrv.Authorizations()
	if len(headers) == 0 {
		t.Fatal("Hugr stub never received an auth-bearing call")
	}
	for _, h := range headers {
		if strings.Contains(h, bootstrap) {
			t.Fatalf("bootstrap secret leaked to Hugr in header %q", h)
		}
		if !strings.Contains(h, jwt) {
			t.Fatalf("expected real JWT %q in header %q", jwt, h)
		}
	}
}

// TestHugrQuery_RejectsMissingHUGR_URL verifies that hugr-query
// fails fast with a clear error when its required env is unset
// (US2 acceptance: failure must be explicit, not a hang).
func TestHugrQuery_RejectsMissingHUGR_URL(t *testing.T) {
	bin := buildBinary(t)
	cmd := exec.Command(bin)
	cmd.Env = []string{} // strip everything — including HUGR_URL
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("hugr-query should have exited non-zero, got success; output=%s", out)
	}
	if !strings.Contains(string(out), "HUGR_URL") {
		t.Fatalf("expected HUGR_URL in stderr, got: %s", out)
	}
}
