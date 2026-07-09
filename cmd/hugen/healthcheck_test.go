package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProbeHealth_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var out bytes.Buffer
	if code := probeHealth(srv.URL+"/healthz", &out); code != 0 {
		t.Fatalf("healthy server: want exit 0, got %d (%s)", code, out.String())
	}
}

func TestProbeHealth_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	var out bytes.Buffer
	if code := probeHealth(srv.URL+"/healthz", &out); code != 1 {
		t.Fatalf("503 server: want exit 1, got %d", code)
	}
	if !strings.Contains(out.String(), "503") {
		t.Errorf("expected status in message, got %q", out.String())
	}
}

func TestProbeHealth_Unreachable(t *testing.T) {
	// Bind then immediately close to get a port nothing listens on.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL + "/healthz"
	srv.Close()

	var out bytes.Buffer
	if code := probeHealth(url, &out); code != 1 {
		t.Fatalf("unreachable server: want exit 1, got %d", code)
	}
}

func TestRunHealthcheck_PortGuard(t *testing.T) {
	for _, tc := range []struct{ name, port string }{
		{"unset", ""},
		{"zero", "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HUGEN_API_PORT", tc.port)
			var out bytes.Buffer
			if code := runHealthcheck(&out); code != 1 {
				t.Fatalf("port %q: want exit 1, got %d", tc.port, code)
			}
			if !strings.Contains(out.String(), "HUGEN_API_PORT") {
				t.Errorf("expected guard message, got %q", out.String())
			}
		})
	}
}
