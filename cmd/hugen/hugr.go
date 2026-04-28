package main

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/hugr-lab/hugen/pkg/identity"
	"github.com/hugr-lab/hugen/pkg/store/local"
	hugr "github.com/hugr-lab/query-engine"
	"github.com/hugr-lab/query-engine/client"
)

func connectRemote(boot *BootstrapConfig, as *auth.Service, logger *slog.Logger) *client.Client {
	ts, ok := as.TokenStore("hugr")
	if !ok {
		logger.Error("failed to get hugr token store")
		return nil
	}
	return client.NewClient(
		boot.Hugr.URL+"/ipc",
		client.WithTransport(auth.Transport(ts, http.DefaultTransport)),
		client.WithTimeout(boot.Hugr.Timeout),
	)
}

func buildLocalEngine(ctx context.Context, config *RuntimeConfig, identity identity.Source, logger *slog.Logger) (*hugr.Service, error) {
	agent, err := identity.Agent(ctx)
	if err != nil {
		return nil, err
	}
	return local.New(ctx, config.LocalDB, local.Identity{
		ID:      agent.ID,
		Type:    agent.AgentTypeID,
		ShortID: agent.ShortID,
		Name:    agent.Name,
	}, config.Embedding, logger)
}
