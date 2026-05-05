package policies

import (
	"context"
	"log/slog"

	"github.com/hugr-lab/hugen/pkg/auth/perm"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// Policies wraps the legacy *tool.Policies behind the new
// package boundary. During phase 4.1a stage A this is the
// agreed migration shape: callers move imports to this package,
// and stage A step 7 swaps composition for native ownership of
// the Save / Revoke / Decide implementations.
//
// Two construction shapes coexist for the duration of the
// interim:
//   - New(inner, perms, log) — wraps an existing *tool.Policies
//     produced by the legacy NewPolicies(querier) factory.
//   - NewFromStore(store, perms, log) — placeholder wired in
//     stage A step 7 once the implementation moves here. Today
//     it returns a configured Policies with a nil inner; calls
//     surface ErrNotImplemented until the move completes.
type Policies struct {
	inner *tool.Policies
	perms perm.Service
	log   *slog.Logger
}

// New wraps a legacy *tool.Policies. Pass nil to construct a
// disabled instance (IsConfigured reports false; tool calls
// surface ErrSystemUnavailable). perms gates the
// hugen:policy:persist permission consulted by callSave /
// callRevoke; nil disables the gate (tests). log captures
// failed-write events; nil falls back to a discard handler.
func New(inner *tool.Policies, perms perm.Service, log *slog.Logger) *Policies {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Policies{inner: inner, perms: perms, log: log}
}

// IsConfigured reports whether the underlying store is wired
// up. Tier-3 callers consult IsConfigured before Decide; tool
// handlers (callSave / callRevoke) bail with
// ErrSystemUnavailable when it returns false.
func (p *Policies) IsConfigured() bool {
	return p.inner.IsConfigured()
}

// Inner exposes the wrapped *tool.Policies for callers that
// still take the legacy type (today: ToolManager.SetPolicies).
// The shortcut retires when the implementation lives natively
// in this subpackage and the manager surface migrates to the
// new pointer type.
func (p *Policies) Inner() *tool.Policies { return p.inner }

// Save persists a policy decision. Wraps the legacy entry —
// signature unchanged so callers can migrate the import without
// any other change.
func (p *Policies) Save(ctx context.Context, in Input) (string, error) {
	return p.inner.Save(ctx, in)
}

// Revoke deletes a Tier-3 policy by composite id. Idempotent.
func (p *Policies) Revoke(ctx context.Context, id string) error {
	return p.inner.Revoke(ctx, id)
}

// Decide consults the in-memory chain (role → skill → global,
// exact then prefix) for the (agentID, toolName, scope) key.
// Used by ToolManager during Resolve.
func (p *Policies) Decide(ctx context.Context, agentID, toolName, scope string) (Decision, error) {
	return p.inner.Decide(ctx, agentID, toolName, scope)
}
