package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/hugr-lab/hugen/pkg/auth/sources/hugr"
)

// authConfig captures the env vars hugr-query reads at boot. The
// runtime sets these when it spawns the binary; missing required
// values are fatal (the operator may add the provider entry but
// forget the env, and a clear error beats a confusing 401 later).
type authConfig struct {
	HugrURL          string
	TokenURL         string
	BootstrapToken   string
	IPCURL           string
}

func loadAuthConfig() (authConfig, error) {
	cfg := authConfig{
		HugrURL:        os.Getenv("HUGR_URL"),
		TokenURL:       os.Getenv("HUGR_TOKEN_URL"),
		BootstrapToken: os.Getenv("HUGR_ACCESS_TOKEN"),
		IPCURL:         os.Getenv("HUGR_IPC_URL"),
	}
	if cfg.HugrURL == "" {
		return cfg, errors.New("HUGR_URL not set")
	}
	if cfg.TokenURL == "" {
		return cfg, errors.New("HUGR_TOKEN_URL not set")
	}
	if cfg.BootstrapToken == "" {
		return cfg, errors.New("HUGR_ACCESS_TOKEN not set")
	}
	return cfg, nil
}

// buildTokenSource constructs the bootstrap-aware RemoteStore. The
// first Token() call falls through to refresh against TokenURL,
// presenting the bootstrap secret in the request body. Subsequent
// calls present the previously-issued JWT.
func buildTokenSource(cfg authConfig) *hugr.RemoteStore {
	return hugr.NewRemoteStoreBootstrap("hugr", cfg.BootstrapToken, cfg.TokenURL)
}

// buildTransport wraps the default round-tripper so every outgoing
// HTTP request to Hugr carries the latest agent JWT. The TokenStore
// caches and refreshes on its own; we ask for the token per call.
func buildTransport(src *hugr.RemoteStore) http.RoundTripper {
	return auth.Transport(src, http.DefaultTransport)
}

// authError formats an auth-source error as a user-facing message
// for tool_error{code:"auth"}. Wraps so callers can errors.Is the
// sentinel.
type authError struct{ err error }

func (e *authError) Error() string { return fmt.Sprintf("auth: %s", e.err.Error()) }
func (e *authError) Unwrap() error { return e.err }

var ErrAuth = errors.New("auth")
