// Command hugr-query is the in-tree Hugr GraphQL MCP server,
// spawned by the runtime over stdio (per-agent lifetime). It
// exposes hugr.Query (writes Parquet for tabular results, JSON
// for objects) and hugr.QueryJQ (post-processes via JQ before
// writing one JSON value).
//
// Auth uses the unmodified pkg/auth/sources/hugr.Source
// configured with TokenURL pointing at the agent's
// /api/auth/agent-token endpoint and a per-spawn bootstrap token
// passed via HUGR_ACCESS_TOKEN env. The agent enforces a 30 s
// bootstrap window plus a per-spawn IssuedHistory LRU so the MCP
// can rotate transparently when the agent's underlying Hugr token
// rotates.
//
// Per-call timeout is read from args.timeout_ms, clamped to
// HUGR_QUERY_MAX_TIMEOUT_MS, with HUGR_QUERY_TIMEOUT_MS as the
// default. Actual elapsed_ms is reported in every success
// envelope.
package main

import (
	"fmt"
	"os"
)

// Stubbed in T001. Real implementation arrives in T046.
func main() {
	fmt.Fprintln(os.Stderr, "hugr-query: not yet implemented (phase-3 task T046)")
	os.Exit(2)
}
