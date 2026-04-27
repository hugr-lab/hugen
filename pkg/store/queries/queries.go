// Package queries is the shared GraphQL runner used by every store
// subpackage. Thin wrappers over types.Querier.Query that auto-close
// the response and surface GraphQL errors.
package queries

import (
	"context"
	"fmt"

	"github.com/hugr-lab/query-engine/types"
)

// RunQuery executes a GraphQL query, closes the response, and scans
// the payload at path into T. Uses ScanData under the hood — the
// unified scan path that honours Arrow extension types (geometry,
// timestamps) and handles embedded row structs / json.RawMessage
// fields through stdlib json.Unmarshal. Fast-path: when Response.Data
// at path is a *types.JsonValue, raw bytes go straight to unmarshal.
func RunQuery[T any](ctx context.Context, q types.Querier, query string, vars map[string]any, path string) (T, error) {
	var zero T
	resp, err := q.Query(ctx, query, vars)
	if err != nil {
		return zero, fmt.Errorf("hubdb query: %w", err)
	}
	defer resp.Close()
	if err := resp.Err(); err != nil {
		return zero, fmt.Errorf("hubdb graphql: %w", err)
	}
	var dest T
	if err := resp.ScanData(path, &dest); err != nil {
		return zero, err
	}
	return dest, nil
}

// RunMutation executes a GraphQL mutation and discards the payload —
// callers use it for writes whose return value is just
// OperationResult.affected_rows.
func RunMutation(ctx context.Context, q types.Querier, mutation string, vars map[string]any) error {
	resp, err := q.Query(ctx, mutation, vars)
	if err != nil {
		return fmt.Errorf("hubdb mutation: %w", err)
	}
	defer resp.Close()
	return resp.Err()
}
