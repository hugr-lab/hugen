// Package template substitutes [$auth.user_id], [$auth.role],
// [$agent.id], [$session.id], and [$session.metadata.<key>]
// placeholders inside JSON string values. Used by
// pkg/auth/perm.Service.Resolve to materialise role-tier
// argument constraints before they reach a tool dispatch.
//
// Substitution is one-pass: a placeholder produced by
// substitution is preserved literally rather than re-substituted,
// to keep the transform terminating and predictable. Missing keys
// substitute to the empty string; malformed `[$...]` tokens are
// preserved verbatim.
package template
