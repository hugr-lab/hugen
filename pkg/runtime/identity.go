package runtime

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/hugr-lab/hugen/pkg/auth"
	"github.com/hugr-lab/hugen/pkg/identity"
	"github.com/hugr-lab/hugen/pkg/identity/hub"
	"github.com/hugr-lab/hugen/pkg/identity/local"

	"github.com/hugr-lab/query-engine/client"
	"github.com/hugr-lab/query-engine/types"
)

// ConnectRemote opens a hugr platform client over the agent's auth
// transport so every outbound request carries the user's bearer
// token. Returns nil and logs when no `hugr` token store is
// registered (no Hugr OIDC configured) — the caller can degrade to
// local-only mode.
func ConnectRemote(cfg HugrConfig, as *auth.Service, logger *slog.Logger) *client.Client {
	ts, ok := as.TokenStore("hugr")
	if !ok {
		logger.Error("failed to get hugr token store")
		return nil
	}
	return client.NewClient(
		cfg.URL+"/ipc",
		client.WithTransport(auth.Transport(ts, http.DefaultTransport)),
		client.WithTimeout(cfg.Timeout),
	)
}

// BuildIdentity selects the identity source for the configured mode:
//
//   - remote (personal-assistant): the agent acts as a hub user, so
//     identity flows from the remote hub through hub.New.
//   - local  (autonomous-agent):   the agent has no hub; identity is
//     read from the local config.yaml at cfg.AgentConfigPath.
func BuildIdentity(cfg Config, remote types.Querier) identity.Source {
	if cfg.Mode == "remote" {
		return hub.New(remote)
	}
	return local.New(cfg.AgentConfigPath)
}

// phaseIdentity runs phase 3: opens an optional remote querier and
// resolves the identity source. Populates Core.RemoteQuerier (only
// when the runtime is remote-mode with a Hugr URL configured) and
// Core.Identity. Registers a cleanup that drains the remote client
// on Shutdown / partial-build failure.
func phaseIdentity(_ context.Context, core *Core) error {
	if core.Cfg.Mode == "remote" && core.Cfg.Hugr.URL != "" {
		core.RemoteQuerier = ConnectRemote(core.Cfg.Hugr, core.Auth, core.Logger)
		if rc := core.RemoteQuerier; rc != nil {
			core.addCleanup(func() { rc.CloseSubscriptions() })
		}
	}
	core.Identity = BuildIdentity(core.Cfg, core.RemoteQuerier)
	return nil
}
