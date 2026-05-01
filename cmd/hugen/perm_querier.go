package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/store/queries"
	"github.com/hugr-lab/query-engine/types"
)

// permQuerier adapts a types.Querier into a perm.Querier that
// runs function.core.auth.my_permissions and maps the response
// to []perm.Rule. The function ignores its argument list — Hugr
// resolves identity from the GraphQL request's Authorization
// header, so QueryRules is parameter-free on the consumer side.
type permQuerier struct {
	q types.Querier
}

func newPermQuerier(q types.Querier) *permQuerier {
	if q == nil {
		return nil
	}
	return &permQuerier{q: q}
}

// permEntry mirrors auth_my_permission_entry from the Hugr
// runtime/auth schema. JSON tags match the GraphQL field names.
type permEntry struct {
	Object   string          `json:"object"`
	Field    string          `json:"field"`
	Hidden   bool            `json:"hidden"`
	Disabled bool            `json:"disabled"`
	Filter   string          `json:"filter,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
}

type myPermissionsResponse struct {
	RoleName    string      `json:"role_name"`
	Disabled    bool        `json:"disabled"`
	Permissions []permEntry `json:"permissions"`
}

// QueryRules fetches the current role's permission rules from
// Hugr and maps them to perm.Rule. An ErrNoData / ErrWrongDataPath
// surface means the caller has no role assignments — that's a
// valid empty list, not an error.
func (p *permQuerier) QueryRules(ctx context.Context) ([]perm.Rule, error) {
	got, err := queries.RunQuery[myPermissionsResponse](ctx, p.q,
		`query {
			function { core { auth { my_permissions {
				role_name disabled
				permissions { object field hidden disabled filter data }
			}}}}
		}`,
		nil,
		"function.core.auth.my_permissions",
	)
	if err != nil {
		if errors.Is(err, types.ErrWrongDataPath) || errors.Is(err, types.ErrNoData) {
			return nil, nil
		}
		return nil, fmt.Errorf("perm: my_permissions: %w", err)
	}
	if got.Disabled {
		// Role itself is disabled — surface as a single
		// catch-all deny so every Resolve fails fast.
		return []perm.Rule{{Type: "*", Field: "*", Disabled: true}}, nil
	}
	rules := make([]perm.Rule, 0, len(got.Permissions))
	for _, e := range got.Permissions {
		rules = append(rules, perm.Rule{
			Type:     e.Object,
			Field:    e.Field,
			Disabled: e.Disabled,
			Hidden:   e.Hidden,
			Filter:   e.Filter,
			Data:     e.Data,
		})
	}
	return rules, nil
}
