package perm

import (
	"context"
	"time"
)

// Service is the consumer-side interface every caller (ToolManager,
// CommandRegistry, SkillManager, MissionPolicy) takes by
// constructor injection. ToolManager holds the concrete
// *LocalPermissions or *RemotePermissions; this interface declares
// the surface fakes implement in tests.
type Service interface {
	// Resolve returns the merged Permission for (object, field)
	// for the given identity. The Permission's Data is already
	// substituted via pkg/auth/template.Apply against the
	// identity.
	Resolve(ctx context.Context, ident Identity, object, field string) (Permission, error)

	// Refresh reloads the underlying snapshot. Local mode is a
	// no-op when config has not changed; Remote mode triggers a
	// singleflight-coalesced bulk fetch from
	// function.core.auth.my_permissions.
	Refresh(ctx context.Context) error

	// Subscribe streams snapshot-change events. Used by
	// ToolManager to bump its (skill_gen, tool_gen, policy_gen)
	// triple on TTL refresh.
	Subscribe(ctx context.Context) (<-chan RefreshEvent, error)
}

// PermissionsView is the consumer-side surface this package
// reads from pkg/config. Declared here per the constitution rule
// (interfaces at the consumer); satisfied structurally by
// *config.StaticService — no explicit pkg/config import on the
// caller side beyond the data types.
type PermissionsView interface {
	Rules() []Rule
	RefreshInterval() time.Duration
	RemoteEnabled() bool
	OnUpdate(fn func()) (cancel func())
}

// Querier is the minimal surface RemotePermissions needs to
// fetch role rules from Hugr. Declared here so the package
// doesn't import pkg/store/local (or query-engine/types
// directly) — production wiring uses a thin adapter, tests use
// a fake.
type Querier interface {
	QueryRules(ctx context.Context, role string) ([]Rule, error)
}
