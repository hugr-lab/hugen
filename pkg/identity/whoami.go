// Package identity resolves the currently authenticated user from
// the hugr server. Used at bootstrap time in remote mode to derive
// the agent_id that keys hub.db.agents without requiring operator
// configuration.
package identity

import (
	"context"
	"errors"
	"fmt"

	"github.com/hugr-lab/query-engine/types"

	"github.com/hugr-lab/hugen/pkg/store/queries"
)

// WhoAmI is the minimal subject description returned by the hugr
// auth.me endpoint. The hugr client has already applied the bearer
// token via its transport, so the query resolves against whatever
// principal the token represents.
type WhoAmI struct {
	UserID   string `json:"user_id"`
	UserName string `json:"user_name"`
}

// ResolveFromHugr executes
//
//	query { function { core { auth { me { user_id user_name } } } } }
//
// and returns the decoded subject. Errors include the usual
// transport / GraphQL failures; an empty user_id also returns an
// error to prevent us from keying hub.db with "".
func ResolveFromHugr(ctx context.Context, q types.Querier) (WhoAmI, error) {
	if q == nil {
		return WhoAmI{}, fmt.Errorf("identity: nil querier")
	}

	const gql = `query {
		function { core { auth { me {
			user_id
			user_name
		} } } }
	}`

	me, err := queries.RunQuery[WhoAmI](ctx, q, gql, nil, "function.core.auth.me")
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return WhoAmI{}, fmt.Errorf("identity: hugr auth.me returned empty payload")
		}
		return WhoAmI{}, fmt.Errorf("identity: resolve whoami: %w", err)
	}
	if me.UserID == "" {
		return WhoAmI{}, fmt.Errorf("identity: hugr returned empty user_id")
	}
	return me, nil
}
