package skill

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewPublisher_DisabledWhenUnconfigured(t *testing.T) {
	if p := NewPublisher("", stubTokenStore("t")); p != nil {
		t.Error("publisher built with no hub URL")
	}
	if p := NewPublisher("http://hub", nil); p != nil {
		t.Error("publisher built with no token store")
	}
	if p := NewPublisher("http://hub", stubTokenStore("t")); p == nil {
		t.Error("publisher not built with valid config")
	}
}

func TestPublisher_Publish_Success(t *testing.T) {
	var gotAuth, gotCT string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(PublishResult{
			Name: "demo", Version: "h-abc", ContentHash: "sha256:abc", Status: "published",
		})
	}))
	defer srv.Close()

	p := NewPublisher(srv.URL, stubTokenStore("tok-123"))
	res, err := p.publish(context.Background(), []byte("BUNDLE"))
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if res.Name != "demo" || res.Version != "h-abc" || res.Status != "published" {
		t.Errorf("unexpected result: %+v", res)
	}
	if gotAuth != "Bearer tok-123" {
		t.Errorf("agent token not forwarded: %q", gotAuth)
	}
	if gotCT != "application/gzip" {
		t.Errorf("content-type = %q, want application/gzip", gotCT)
	}
	if string(gotBody) != "BUNDLE" {
		t.Errorf("body = %q, want BUNDLE", gotBody)
	}
}

func TestPublisher_Publish_SurfacesHubError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"code": "reserved_name", "message": "name is reserved by a bundled skill"},
		})
	}))
	defer srv.Close()

	p := NewPublisher(srv.URL, stubTokenStore("t"))
	_, err := p.publish(context.Background(), []byte("x"))
	if err == nil {
		t.Fatal("expected an error on 403")
	}
	if got := err.Error(); !strings.Contains(got, "reserved_name") || !strings.Contains(got, "reserved by a bundled skill") {
		t.Errorf("error did not surface the hub message: %q", got)
	}
}

// stubTokenStore satisfies auth.TokenStore, returning a fixed token.
type stubTokenStore string

func (s stubTokenStore) Token(context.Context) (string, error) { return string(s), nil }
