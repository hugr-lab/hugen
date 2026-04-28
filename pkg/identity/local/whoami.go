// Package identity resolves the currently authenticated user from
// the hugr server. Used at bootstrap time in remote mode to derive
// the agent_id that keys hub.db.agents without requiring operator
// configuration.
package local

import (
	"context"

	"github.com/hugr-lab/hugen/pkg/identity"
)

// WhoAmI executes
//
//	query { function { core { auth { me { user_id user_name } } } } }
//
// and returns the decoded subject. Errors include the usual
// transport / GraphQL failures; an empty user_id also returns an
// error to prevent us from keying hub.db with "".
func (s *Source) WhoAmI(ctx context.Context) (identity.WhoAmI, error) {
	// If the hub source is nil, we are likely running in local mode.
	if s.hub == nil {
		return identity.WhoAmI{
			UserID:   "local",
			UserName: "local",
			Role:     "local",
		}, nil
	}

	return s.hub.WhoAmI(ctx)
}
