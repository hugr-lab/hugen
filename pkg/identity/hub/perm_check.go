package hub

import (
	"context"

	"github.com/hugr-lab/hugen/pkg/identity"
	"github.com/hugr-lab/hugen/pkg/store/queries"
)

func (source *Source) Permission(ctx context.Context, section, name string) (identity.Permission, error) {
	return queries.RunQuery[identity.Permission](ctx, source.qe, `query ($section: String!, $name: String!) {
		function { core { auth { check_access_info(type_name: $section, field: $name){
          enabled
          data
          filter
        } } } }
	}`, map[string]any{
		"section": section,
		"name":    name,
	}, "function.core.auth.check_access_info")
}
