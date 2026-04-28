package webui

import (
	"io/fs"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestStaticFS_Embedded asserts the embedded FS exposes the three
// SPA files (T015). Tests reach into the embed.FS directly so a
// missing build-time embed surfaces as a fast failure rather than a
// runtime 404.
func TestStaticFS_Embedded(t *testing.T) {
	for _, path := range []string{"index.html", "app.js", "app.css"} {
		if _, err := fs.Stat(StaticFS(), path); err != nil {
			t.Errorf("static FS missing %s: %v", path, err)
		}
	}
}

// TestAdapter_ServesIndex asserts GET / returns 200 with the
// templated index.html, including the operator-facing markers
// (page title, /api/v1 reference) and the templated meta tag (T014).
func TestAdapter_ServesIndex(t *testing.T) {
	a := NewAdapter("127.0.0.1", 0, "http://127.0.0.1:10000", nil)
	srv := httptest.NewServer(stdhttp.HandlerFunc(a.serve))
	t.Cleanup(srv.Close)

	resp, err := srv.Client().Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != stdhttp.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("Content-Type = %q", got)
	}
	body := readBody(t, resp)
	for _, want := range []string{"hugen", "/api/v1", "http://127.0.0.1:10000"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

// TestAdapter_StaticAssetsServed asserts GET /app.js returns the
// embedded JS with a JavaScript-ish content-type. Same for app.css.
func TestAdapter_StaticAssetsServed(t *testing.T) {
	a := NewAdapter("127.0.0.1", 0, "http://127.0.0.1:10000", nil)
	srv := httptest.NewServer(stdhttp.HandlerFunc(a.serve))
	t.Cleanup(srv.Close)

	cases := []struct {
		path string
		want string // substring of Content-Type
	}{
		{"/app.js", "javascript"},
		{"/app.css", "css"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			resp, err := srv.Client().Get(srv.URL + tc.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != stdhttp.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if got := resp.Header.Get("Content-Type"); !strings.Contains(got, tc.want) {
				t.Errorf("Content-Type = %q, want substring %q", got, tc.want)
			}
		})
	}
}

// TestAdapter_NoAuthOnLoopback — the static page is loopback-only
// and bypasses auth (FR-015). Asserting "no Authorization required"
// on the webui listener; the API listener auth is covered by
// pkg/adapter/http auth tests.
func TestAdapter_NoAuthOnLoopback(t *testing.T) {
	a := NewAdapter("127.0.0.1", 0, "http://127.0.0.1:10000", nil)
	srv := httptest.NewServer(stdhttp.HandlerFunc(a.serve))
	t.Cleanup(srv.Close)

	req, err := stdhttp.NewRequest("GET", srv.URL+"/", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	// No Authorization header.
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != stdhttp.StatusOK {
		t.Errorf("static page rejected without auth: status = %d", resp.StatusCode)
	}
}

func readBody(t *testing.T, resp *stdhttp.Response) string {
	t.Helper()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			sb.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	return sb.String()
}
