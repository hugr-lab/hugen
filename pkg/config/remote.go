package config

import (
	"context"
	"fmt"

	"github.com/hugr-lab/query-engine/types"
	"github.com/spf13/viper"

	agentstore "github.com/hugr-lab/hugen/pkg/agent/store"
)

// LoadRemote pulls the agent's config from hub.db.agents (merging
// agent_types.config defaults with agents.config_override) and
// returns a typed *Config. The bootstrap already has hugr + a2a +
// devui fields filled in from .env; those take precedence over
// whatever the hub returned for the same keys.
//
// Fails loudly if the agent row is missing — in remote mode hub
// is the authoritative registry, so the absence of a row signals
// misconfiguration rather than a condition we should paper over.
func LoadRemote(ctx context.Context, q types.Querier, agentID string, boot *BootstrapConfig) (*Config, error) {
	if boot == nil {
		return nil, fmt.Errorf("config: LoadRemote requires BootstrapConfig")
	}
	if q == nil {
		return nil, fmt.Errorf("config: LoadRemote requires a querier")
	}
	if agentID == "" {
		return nil, fmt.Errorf("config: LoadRemote requires non-empty agentID")
	}

	merged, agentRow, err := agentstore.LoadConfigFromHub(ctx, q, agentID)
	if err != nil {
		return nil, err
	}

	v := viper.New()
	if err := v.MergeConfigMap(merged); err != nil {
		return nil, fmt.Errorf("config: merge remote config map: %w", err)
	}

	cfg := &Config{
		Hugr:     boot.Hugr,
		A2A:      boot.A2A,
		DevUI:    boot.DevUI,
		Identity: boot.Identity,
	}
	if err := decodeAndFinalize(v, cfg); err != nil {
		return nil, fmt.Errorf("config: decode remote: %w", err)
	}

	// Identity fields that came from agents row — they're not part
	// of the config shape but needed for the runtime.
	if cfg.Identity.ID == "" {
		cfg.Identity.ID = agentRow.ID
	}
	if cfg.Identity.ShortID == "" {
		cfg.Identity.ShortID = agentRow.ShortID
	}
	if cfg.Identity.Name == "" {
		cfg.Identity.Name = agentRow.Name
	}
	if cfg.Identity.Type == "" {
		cfg.Identity.Type = agentRow.AgentTypeID
	}
	return cfg, nil
}
