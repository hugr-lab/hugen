package hub

import (
	"context"

	"github.com/hugr-lab/hugen/pkg/identity"
	"github.com/hugr-lab/hugen/pkg/store/queries"
)

// Agent retrieves the agent with the given id from hub
// If it is allowed, agent will create automatically by this request
// Agent config will automatically merge with agent_type config, and stored in hub.db.agents.config
func (c *Source) Agent(ctx context.Context) (identity.Agent, error) {
	return queries.RunQuery[identity.Agent](ctx, c.qe,
		`mutation {
			hub {
				agent_info {
					id
					agent_type_id
					short_id
					name
					status
					config
				}
			}
		}`,
		nil,
		"hub.agent_info",
	)
}
