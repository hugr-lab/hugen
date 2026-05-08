//go:build duckdb_arrow && scenario

package harness

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// RunQuery executes one Query clause against the runtime's local
// hugr Querier (the same engine the agent persists into) and dumps
// the response into t.Log. Pass-through `$sid` substitution: when
// q.Vars references a literal "$sid" string, it's replaced with
// the live session id. Failures are logged, not fatal — this is
// observational.
func (r *Runtime) RunQuery(ctx context.Context, t *testing.T, sessionID string, q Query) {
	t.Helper()
	if q.Name == "" {
		q.Name = "(unnamed)"
	}
	t.Logf("=== query %s ===", q.Name)

	vars := make(map[string]any, len(q.Vars))
	for k, v := range q.Vars {
		if s, ok := v.(string); ok && s == "$sid" {
			vars[k] = sessionID
		} else {
			vars[k] = v
		}
	}

	resp, err := r.Core.LocalQuerier.Query(ctx, q.GraphQL, vars)
	if err != nil {
		t.Logf("query %q errored: %v", q.Name, err)
		return
	}
	defer resp.Close()

	if len(resp.Errors) > 0 {
		for _, e := range resp.Errors {
			t.Logf("[graphql error] %s", e.Message)
		}
	}

	// Default render: extract by Path or dump the whole envelope.
	// Path supports the conventional "data.x.y" / "extensions.jq.jq"
	// dotted addressing.
	if q.Path == "" {
		dumpJSON(t, q.Name, map[string]any{
			"data":       resp.Data,
			"extensions": resp.Extensions,
		})
		return
	}

	// Manual path walk so callers can address either side of the
	// envelope (response.Data vs response.Extensions). The Querier's
	// own ScanData* helpers only walk Data.
	root := map[string]any{
		"data":       resp.Data,
		"extensions": resp.Extensions,
	}
	leaf, ok := walkPath(root, q.Path)
	if !ok {
		t.Logf("path %q not found in response", q.Path)
		dumpJSON(t, q.Name+" (full)", root)
		return
	}
	dumpJSON(t, q.Name, leaf)
}

// walkPath descends a dotted path through a map[string]any tree.
// Returns (leaf, true) on success, (nil, false) when any segment
// is missing or a non-map.
func walkPath(root any, path string) (any, bool) {
	cur := root
	for _, seg := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		next, ok := m[seg]
		if !ok {
			return nil, false
		}
		cur = next
	}
	return cur, true
}

// dumpJSON renders the value as multi-line indented JSON into
// t.Log. Marshal failures fall back to %v formatting so the
// scenario log always carries something.
func dumpJSON(t *testing.T, label string, v any) {
	t.Helper()
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Logf("[%s] marshal error: %v; raw: %v", label, err, v)
		return
	}
	for _, line := range strings.Split(string(body), "\n") {
		t.Logf("    %s", line)
	}
}
