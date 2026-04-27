package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/hugr-lab/query-engine/types"

	"github.com/hugr-lab/hugen/pkg/store/queries"
)

// LoadConfigFromHub fetches an agent row together with its
// agent_type's default config, then merges them top-level:
//
//	result = agent_type.config  (defaults)
//	        ⊕ agents.config_override   (shallow overlay)
//
// Returns the merged map suitable for mapstructure.Decode into the
// application Config, plus the resolved agent_type_id so callers
// can fill in identity fields that aren't part of the config shape.
//
// Used at bootstrap in remote mode, before the full Client is
// constructed — operates directly on a types.Querier.
func LoadConfigFromHub(ctx context.Context, q types.Querier, agentID string) (map[string]any, *Agent, error) {
	if q == nil {
		return nil, nil, fmt.Errorf("agent store: nil querier")
	}
	if agentID == "" {
		return nil, nil, fmt.Errorf("agent store: empty agent ID")
	}

	agent, err := fetchAgent(ctx, q, agentID)
	if err != nil {
		return nil, nil, err
	}
	if agent == nil {
		return nil, nil, fmt.Errorf("agent store: agent %q not registered in hub — run hub-side registration first", agentID)
	}

	typeCfg, err := fetchAgentTypeConfig(ctx, q, agent.AgentTypeID)
	if err != nil {
		return nil, nil, err
	}

	merged := mergeTopLevel(typeCfg, agent.ConfigOverride)
	return merged, agent, nil
}

// mergeTopLevel returns a shallow merge of base and overlay by
// top-level keys. overlay wins where both define a key. Neither
// input is mutated. A nil map is treated as empty.
func mergeTopLevel(base, overlay map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(overlay))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overlay {
		out[k] = v
	}
	return out
}

// fetchAgent is LoadConfigFromHub's internal variant of GetAgent —
// same query, no Client dependency.
func fetchAgent(ctx context.Context, q types.Querier, id string) (*Agent, error) {
	type row struct {
		ID             string         `json:"id"`
		AgentTypeID    string         `json:"agent_type_id"`
		ShortID        string         `json:"short_id"`
		Name           string         `json:"name"`
		Status         string         `json:"status"`
		ConfigOverride map[string]any `json:"config_override"`
	}
	rows, err := queries.RunQuery[[]row](ctx, q,
		`query ($id: String!) {
			hub { db {
				agents(filter: {id: {eq: $id}}, limit: 1) {
					id agent_type_id short_id name status config_override
				}
			}}
		}`,
		map[string]any{"id": id},
		"hub.db.agents",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return nil, nil
		}
		return nil, fmt.Errorf("agent store: fetch agent %q: %w", id, err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	r := rows[0]
	return &Agent{
		ID:             r.ID,
		AgentTypeID:    r.AgentTypeID,
		ShortID:        r.ShortID,
		Name:           r.Name,
		Status:         r.Status,
		ConfigOverride: r.ConfigOverride,
	}, nil
}

// fetchAgentTypeConfig returns just the config map for a given
// agent_type_id; identity fields aren't used at load time.
func fetchAgentTypeConfig(ctx context.Context, q types.Querier, typeID string) (map[string]any, error) {
	type row struct {
		Config map[string]any `json:"config"`
	}
	rows, err := queries.RunQuery[[]row](ctx, q,
		`query ($id: String!) {
			hub { db {
				agent_types(filter: {id: {eq: $id}}, limit: 1) {
					config
				}
			}}
		}`,
		map[string]any{"id": typeID},
		"hub.db.agent_types",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return nil, fmt.Errorf("agent store: agent_type %q not found in hub", typeID)
		}
		return nil, fmt.Errorf("agent store: fetch agent_type %q: %w", typeID, err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("agent store: agent_type %q not found in hub", typeID)
	}
	if rows[0].Config == nil {
		return map[string]any{}, nil
	}
	return rows[0].Config, nil
}
