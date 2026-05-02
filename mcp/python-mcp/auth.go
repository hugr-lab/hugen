package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/hugr-lab/hugen/pkg/auth/sources/hugr"
)

// authSource captures the env-var contract documented in
// contracts/python-mcp.md: HUGR_URL + HUGR_ACCESS_TOKEN +
// HUGR_TOKEN_URL together activate the bootstrap-aware
// RemoteStore. Any subset is treated as "no Hugr" with a warning
// (US5 path).
type authSource struct {
	hugrURL string
	store   *hugr.RemoteStore
}

// loadAuthSource reads the env contract once at server start.
// Returns (nil, nil) on the no-Hugr path so callers can branch
// with `auth == nil`.
func loadAuthSource(log *slog.Logger) (*authSource, error) {
	url := os.Getenv("HUGR_URL")
	bootstrap := os.Getenv("HUGR_ACCESS_TOKEN")
	tokenURL := os.Getenv("HUGR_TOKEN_URL")

	switch {
	case url == "" && bootstrap == "" && tokenURL == "":
		log.Info("python-mcp: Hugr env unset — running without Hugr credentials")
		return nil, nil
	case url != "" && bootstrap != "" && tokenURL != "":
		return &authSource{
			hugrURL: url,
			store:   hugr.NewRemoteStoreBootstrap("hugr", bootstrap, tokenURL),
		}, nil
	default:
		log.Warn("python-mcp: partial Hugr env (need HUGR_URL+HUGR_ACCESS_TOKEN+HUGR_TOKEN_URL); treating as unset",
			"have_url", url != "",
			"have_bootstrap", bootstrap != "",
			"have_token_url", tokenURL != "")
		return nil, nil
	}
}

// currentToken refreshes against the loopback agent-token endpoint
// when the store is configured; returns ("", "") on the no-Hugr
// path so the per-call env builder can branch cleanly.
func (a *authSource) currentToken(ctx context.Context) (url, token string, err error) {
	if a == nil || a.store == nil {
		return "", "", nil
	}
	t, err := a.store.Token(ctx)
	if err != nil {
		return "", "", err
	}
	return a.hugrURL, t, nil
}
