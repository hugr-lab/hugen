package tool

import "context"

// Tier-3 personal tool policies — the contract ToolManager.Resolve
// consults. The store and the LLM-facing tool surface
// (policy:save / policy:revoke) live in pkg/tool/providers/policies;
// pkg/tool only holds the small interface + decision types so the
// manager stays free of pkg/store/queries.
//
// Tier never overrides Tier-1 or Tier-2 — Resolve only consults
// Decide AFTER the upstream check returned non-disabled. Outcomes:
//
//   - PolicyAllow → run without prompting (sets FromUser=true).
//   - PolicyDeny  → block; ToolManager surfaces ErrPermissionDenied
//     with tier=user.
//   - PolicyAsk   → no opinion; defer to the default policy
//     (phase-5 HITL approval lands here; phase-3 treats Ask as
//     allow because the floor already approved the call).

// PolicyOutcome is the persisted decision enum.
type PolicyOutcome int

const (
	PolicyAsk PolicyOutcome = iota
	PolicyAllow
	PolicyDeny
)

// String returns the on-disk representation expected by the
// tool_policies.policy column.
func (o PolicyOutcome) String() string {
	switch o {
	case PolicyAllow:
		return "always_allowed"
	case PolicyDeny:
		return "denied"
	default:
		return "manual_required"
	}
}

// PolicyDecision is the value Decide returns. Reason carries a
// short human-readable description of the matched row (used in
// audit frames and tool_error envelopes).
type PolicyDecision struct {
	Outcome  PolicyOutcome
	ToolName string
	Scope    string
	Reason   string
}

// PolicyService is the Tier-3 façade ToolManager consults during
// Resolve. The concrete implementation lives in
// pkg/tool/providers/policies; pkg/tool depends only on the
// interface so the manager package stays free of the
// persistence-layer imports the implementation needs
// (pkg/store/queries, types.Querier).
//
// IsConfigured is nil-safe: callers may pass a nil PolicyService
// and pkg/tool treats it as "Tier-3 disabled" without panicking.
type PolicyService interface {
	// IsConfigured reports whether Tier-3 consultation is wired.
	// nil receivers must return false.
	IsConfigured() bool

	// Decide consults the persisted rows for `agentID`, evaluating
	// the most-specific match against `toolName` honouring `*`-suffix
	// globs. Returns a zero PolicyDecision (Outcome == PolicyAsk)
	// when no row matches.
	Decide(ctx context.Context, agentID, toolName, scope string) (PolicyDecision, error)
}
