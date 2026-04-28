// Package http exposes the agent runtime as a JSON+SSE API under
// /api/v1. See specs/002-agent-runtime-phase-2/contracts/http-api.md
// and contracts/sse-wire-format.md for the full contract.
//
// The adapter mounts on a shared *http.ServeMux owned by the
// auth.Service so existing bearer middleware applies to every
// /api/v1/* request without re-implementation.
package http
