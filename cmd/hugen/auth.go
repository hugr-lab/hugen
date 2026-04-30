package main

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/hugr-lab/hugen/pkg/auth/sources/hugr"
)

func buildAuthService(ctx context.Context, boot *BootstrapConfig, mux *http.ServeMux, logger *slog.Logger) (*auth.Service, error) {
	as := auth.NewService(logger, mux, boot.BaseURI, boot.IsRemoteMode())
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
	return as, nil
}
