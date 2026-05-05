package policies

import (
	"context"

	"github.com/hugr-lab/hugen/pkg/tool"
)

// Forward-looking type aliases so callers reach the Tier-3
// vocabulary through this package while the implementation
// lives in pkg/tool. Stage A step 7 relocates the underlying
// types here and these aliases retire.
type (
	Input    = tool.PolicyInput
	Decision = tool.PolicyDecision
	Outcome  = tool.PolicyOutcome
)

// Store is the persistence boundary Policies sits on top of.
// Stage A step 5 declares the contract; the DuckDB-backed
// implementation (`pkg/store/queries/policies_store.go`) and
// the actual decoupling of Policies from `types.Querier` land
// in stage A step 7. Until then the implementation continues
// to use the legacy querier-coupled path inside *tool.Policies.
//
// Method shapes mirror the operations the legacy implementation
// performs — Save (UPSERT), Revoke (delete by composite id),
// Load (read by agent for the in-memory decision tree).
type Store interface {
	// Save inserts or updates a Tier-3 policy row. The composite
	// key is (Input.AgentID, Input.ToolName, Input.Scope). Returns
	// the canonical row id (parsable via parsePolicyID).
	Save(ctx context.Context, in Input) (string, error)

	// Revoke deletes the row keyed by composite id. Idempotent —
	// missing rows return nil.
	Revoke(ctx context.Context, id string) error

	// Load returns every policy row owned by agentID. Order does
	// not matter — Decide builds a chain in memory.
	Load(ctx context.Context, agentID string) ([]PolicyRow, error)
}

// PolicyRow is the Store's read shape — one row in the
// tool_policies table. Mirrors pkg/tool's internal policyRow but
// exported here so the Store interface can stand alone. Stage A
// step 7 collapses the duplicate when the legacy code retires.
type PolicyRow struct {
	ID       string
	AgentID  string
	ToolName string
	Scope    string
	Outcome  Outcome
	Note     string
}
