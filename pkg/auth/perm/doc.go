// Package perm implements the permission service: a Service
// interface plus two concrete implementations.
//
//   - LocalPermissions resolves entirely from operator config
//     (Tier 1). No Hugr round-trip; Refresh is a no-op.
//   - RemotePermissions layers Hugr role rules (Tier 2) on top
//     of the Tier-1 floor. TTL-cached per role; concurrent
//     refreshes coalesced via singleflight; refresh failure
//     preserves the previous snapshot until either next refresh
//     succeeds or hard expiry kicks in.
//
// Merge semantics (Tier 1 + Tier 2): Disabled OR'd, Hidden OR'd,
// Data shallow-merged with config-wins on scalar conflict, Filter
// AND-merged for GraphQL-shaped tools. Tier 3 (user
// tool_policies) is consulted by ToolManager AFTER perm.Service —
// it cannot relax Tier-1 / Tier-2 Disabled.
//
// PermissionsView is declared here (consumer-side) so the package
// doesn't import pkg/config and can be tested with a fake.
package perm
