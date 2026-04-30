package main

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/hugr-lab/hugen/pkg/auth/sources/hugr"
	"github.com/hugr-lab/hugen/pkg/auth/sources/oidc"
)

func buildAuthService(ctx context.Context, boot *BootstrapConfig, mux *http.ServeMux, logger *slog.Logger) (*auth.Service, error) {
	as := auth.NewService(logger, mux, boot.BaseURI)
	if boot.Hugr.URL == "" {
		logger.Info("no hugr auth config; skipping hugr auth source")
		return as, nil
	}
	hugrAuth, err := hugr.BuildHugrSource(ctx, hugr.Config{
		BaseURI:     boot.BaseURI,
		RedirectURI: boot.Hugr.RedirectURI,
		DiscoverURL: boot.Hugr.URL,
		AccessToken: boot.Hugr.AccessToken,
		TokenURL:    boot.Hugr.TokenURL,
		Issuer:      boot.Hugr.Issuer,
		ClientID:    boot.Hugr.ClientID,
	}, logger)
	if err != nil {
		return nil, err
	}

	if err := as.AddPrimary(hugrAuth); err != nil {
		return nil, err
	}
	// In local-OIDC mode the primary source needs a browser flow to
	// obtain the first token. Register its PromptLogin so the URL is
	// printed once the HTTP listener is bound; runtime.go fires the
	// queue right after this returns.
	if oidcSrc, ok := hugrAuth.(*oidc.Source); ok {
		as.RegisterPromptLogin(oidcSrc.PromptLogin)
	}
	return as, nil
}
