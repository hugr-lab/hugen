package http

import (
	"bytes"
	"context"
	"encoding/json"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/runtime"
)

// newTestServer wires up an Adapter against a fakeHost on a fresh
// httptest.Server. Returned cleanup must be deferred.
func newTestServer(t *testing.T, auth Authenticator) (*fakeHost, *httptest.Server) {
	t.Helper()
	host := newFakeHost()
	mux := stdhttp.NewServeMux()
	a, err := NewAdapter(Options{
		Mux:    mux,
		Auth:   auth,
		Codec:  protocol.NewCodec(),
		Replay: host.store,
	})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	// Drive Run on a background context so handlers are mounted.
	// Block on Mounted() so the test never reads routes before
	// Run has had a chance to register them.
	runCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = a.Run(runCtx, host) }()
	<-a.Mounted()
	a.MarkReady()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return host, srv
}

func authHeader(token string) stdhttp.Header {
	h := stdhttp.Header{}
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	return h
}

// doJSON issues an authenticated JSON request and returns the
// response — the caller is responsible for closing the body.
func doJSON(t *testing.T, srv *httptest.Server, method, path, token string, body any) *stdhttp.Response {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	req, err := stdhttp.NewRequest(method, srv.URL+path, &buf)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header = authHeader(token)
	if buf.Len() > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

func TestHandlers_OpenSession(t *testing.T) {
	_, srv := newTestServer(t, allowAllAuth{})
	resp := doJSON(t, srv, "POST", "/api/v1/sessions", "tok", nil)
	defer resp.Body.Close()
	if resp.StatusCode != stdhttp.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	var body OpenSessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.SessionID == "" {
		t.Errorf("session_id is empty")
	}
	if body.Status != runtime.StatusActive {
		t.Errorf("status = %q, want %q", body.Status, runtime.StatusActive)
	}
	if body.OpenedAt.IsZero() {
		t.Errorf("opened_at is zero")
	}
}

func TestHandlers_PostUserMessage(t *testing.T) {
	_, srv := newTestServer(t, allowAllAuth{})

	openResp := doJSON(t, srv, "POST", "/api/v1/sessions", "tok", nil)
	var open OpenSessionResponse
	_ = json.NewDecoder(openResp.Body).Decode(&open)
	openResp.Body.Close()

	body := map[string]any{
		"kind":    "user_message",
		"author":  map[string]any{"id": "alice", "kind": "user"},
		"payload": map[string]any{"text": "hi"},
	}
	resp := doJSON(t, srv, "POST", "/api/v1/sessions/"+open.SessionID+"/post", "tok", body)
	defer resp.Body.Close()
	if resp.StatusCode != stdhttp.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	var out PostFrameResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.FrameID == "" {
		t.Errorf("frame_id is empty")
	}
	if out.SessionID != open.SessionID {
		t.Errorf("session_id = %q, want %q", out.SessionID, open.SessionID)
	}
}

func TestHandlers_PostRejectsClientSetIdentifiers(t *testing.T) {
	_, srv := newTestServer(t, allowAllAuth{})
	openResp := doJSON(t, srv, "POST", "/api/v1/sessions", "tok", nil)
	var open OpenSessionResponse
	_ = json.NewDecoder(openResp.Body).Decode(&open)
	openResp.Body.Close()

	cases := []string{"frame_id", "session_id", "occurred_at"}
	for _, field := range cases {
		t.Run(field, func(t *testing.T) {
			body := map[string]any{
				"kind":    "user_message",
				"author":  map[string]any{"id": "alice", "kind": "user"},
				"payload": map[string]any{"text": "hi"},
				field:     "client-set-value",
			}
			resp := doJSON(t, srv, "POST",
				"/api/v1/sessions/"+open.SessionID+"/post", "tok", body)
			defer resp.Body.Close()
			if resp.StatusCode != stdhttp.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			var env ErrorEnvelope
			_ = json.NewDecoder(resp.Body).Decode(&env)
			if env.Error.Code != "client_set_field" {
				t.Errorf("code = %q, want client_set_field", env.Error.Code)
			}
		})
	}
}

func TestHandlers_PostRejectsForbiddenKinds(t *testing.T) {
	_, srv := newTestServer(t, allowAllAuth{})
	openResp := doJSON(t, srv, "POST", "/api/v1/sessions", "tok", nil)
	var open OpenSessionResponse
	_ = json.NewDecoder(openResp.Body).Decode(&open)
	openResp.Body.Close()

	for _, kind := range []string{"agent_message", "tool_call", "system_marker"} {
		t.Run(kind, func(t *testing.T) {
			body := map[string]any{
				"kind":    kind,
				"author":  map[string]any{"id": "alice", "kind": "user"},
				"payload": map[string]any{"text": "hi"},
			}
			resp := doJSON(t, srv, "POST",
				"/api/v1/sessions/"+open.SessionID+"/post", "tok", body)
			defer resp.Body.Close()
			if resp.StatusCode != stdhttp.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			var env ErrorEnvelope
			_ = json.NewDecoder(resp.Body).Decode(&env)
			if env.Error.Code != "invalid_kind" {
				t.Errorf("code = %q, want invalid_kind", env.Error.Code)
			}
		})
	}
}

func TestHandlers_CloseIdempotent(t *testing.T) {
	host, srv := newTestServer(t, allowAllAuth{})
	openResp := doJSON(t, srv, "POST", "/api/v1/sessions", "tok", nil)
	var open OpenSessionResponse
	_ = json.NewDecoder(openResp.Body).Decode(&open)
	openResp.Body.Close()

	resp1 := doJSON(t, srv, "POST", "/api/v1/sessions/"+open.SessionID+"/close", "tok",
		map[string]any{"reason": "user_end"})
	if resp1.StatusCode != stdhttp.StatusOK {
		t.Fatalf("first close status = %d, want 200", resp1.StatusCode)
	}
	resp1.Body.Close()
	resp2 := doJSON(t, srv, "POST", "/api/v1/sessions/"+open.SessionID+"/close", "tok",
		map[string]any{"reason": "again"})
	if resp2.StatusCode != stdhttp.StatusOK {
		t.Fatalf("second close status = %d, want 200", resp2.StatusCode)
	}
	resp2.Body.Close()
	if got := host.sessions[open.SessionID].Status; got != runtime.StatusClosed {
		t.Errorf("status after close = %q, want %q", got, runtime.StatusClosed)
	}
}

func TestHandlers_ListSessions_Filter(t *testing.T) {
	host, srv := newTestServer(t, allowAllAuth{})

	// Create one active and one closed session directly through the
	// fake host so we don't depend on the close handler.
	active, _, _ := host.OpenSession(context.Background(), runtime.OpenRequest{})
	closed, _, _ := host.OpenSession(context.Background(), runtime.OpenRequest{})
	_, _ = host.CloseSession(context.Background(), closed.ID(), "test")

	resp := doJSON(t, srv, "GET", "/api/v1/sessions?status=active", "tok", nil)
	defer resp.Body.Close()
	if resp.StatusCode != stdhttp.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body ListSessionsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Sessions) != 1 {
		t.Fatalf("got %d sessions, want 1 (%v)", len(body.Sessions), body.Sessions)
	}
	if body.Sessions[0].SessionID != active.ID() {
		t.Errorf("returned %q, want active %q", body.Sessions[0].SessionID, active.ID())
	}
}

func TestHandlers_CloseUnknownReturns404(t *testing.T) {
	_, srv := newTestServer(t, allowAllAuth{})
	resp := doJSON(t, srv, "POST", "/api/v1/sessions/ses-does-not-exist/close", "tok",
		map[string]any{"reason": "test"})
	defer resp.Body.Close()
	if resp.StatusCode != stdhttp.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	var env ErrorEnvelope
	_ = json.NewDecoder(resp.Body).Decode(&env)
	if env.Error.Code != "session_not_found" {
		t.Errorf("code = %q, want session_not_found", env.Error.Code)
	}
}

func TestHandlers_PostMetadata_RoundTrip(t *testing.T) {
	host, srv := newTestServer(t, allowAllAuth{})
	resp := doJSON(t, srv, "POST", "/api/v1/sessions", "tok", map[string]any{
		"metadata": map[string]any{"label": "investigation"},
	})
	defer resp.Body.Close()
	var open OpenSessionResponse
	_ = json.NewDecoder(resp.Body).Decode(&open)
	row, ok := host.sessions[open.SessionID]
	if !ok {
		t.Fatalf("session %q not stored", open.SessionID)
	}
	if got, _ := row.Metadata["label"]; got != "investigation" {
		t.Errorf("metadata.label = %v, want %q", got, "investigation")
	}
}

func TestHandlers_PostFrame_PayloadTooLarge(t *testing.T) {
	// Tiny limit so a single big string trips the gate. The
	// production default is 64 KiB.
	host := newFakeHost()
	mux := stdhttp.NewServeMux()
	a, err := NewAdapter(Options{
		Mux:             mux,
		Auth:            allowAllAuth{},
		Codec:           protocol.NewCodec(),
		Replay:          host.store,
		MaxRequestBytes: 64,
	})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = a.Run(runCtx, host) }()
	<-a.Mounted()
	a.MarkReady()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	openResp := doJSON(t, srv, "POST", "/api/v1/sessions", "tok", nil)
	var open OpenSessionResponse
	_ = json.NewDecoder(openResp.Body).Decode(&open)
	openResp.Body.Close()

	big := strings.Repeat("x", 1024)
	resp := doJSON(t, srv, "POST", "/api/v1/sessions/"+open.SessionID+"/post", "tok", map[string]any{
		"kind":    "user_message",
		"author":  map[string]any{"id": "alice", "kind": "user"},
		"payload": map[string]any{"text": big},
	})
	defer resp.Body.Close()
	if resp.StatusCode != stdhttp.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}

func TestHandlers_BadStatusQuery(t *testing.T) {
	_, srv := newTestServer(t, allowAllAuth{})
	resp := doJSON(t, srv, "GET", "/api/v1/sessions?status=ZZZ", "tok", nil)
	defer resp.Body.Close()
	if resp.StatusCode != stdhttp.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var env ErrorEnvelope
	_ = json.NewDecoder(resp.Body).Decode(&env)
	if !strings.Contains(env.Error.Code, "bad_query") {
		t.Errorf("code = %q, want bad_query", env.Error.Code)
	}
}
