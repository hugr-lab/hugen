package local

import (
	"context"

	"github.com/hugr-lab/hugen/pkg/identity"
)

func (source *Source) Permission(ctx context.Context, section, name string) (identity.Permission, error) {
	if source.hub == nil {
		return identity.Permission{
			Enabled: true,
		}, nil
	}
	return source.hub.Permission(ctx, section, name)
}
