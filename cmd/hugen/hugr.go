package main

import (
	"context"
	"log/slog"

	"github.com/hugr-lab/hugen/pkg/config"
	"github.com/hugr-lab/hugen/pkg/identity"
	"github.com/hugr-lab/hugen/pkg/store/local"

	hugr "github.com/hugr-lab/query-engine"
)

func buildLocalEngine(
	ctx context.Context,
	localView config.LocalView,
	embedView config.EmbeddingView,
	idSrc identity.Source,
	logger *slog.Logger,
) (*hugr.Service, error) {
	return local.New(ctx, localView, embedView, idSrc, logger)
}
