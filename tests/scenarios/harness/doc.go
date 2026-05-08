// Package harness implements the phase-4.1b observational scenario
// harness. It is test-only; both the duckdb_arrow and scenario build
// tags are required to compile this package, which keeps it out of
// every "go build ./..." path the operator typically runs.
//
// The harness is not a regression suite. It boots a real *runtime.Core
// per run, drives one or more YAML-described scenarios against it
// (live LLM, optionally a real Hugr endpoint), dumps the resulting
// session_events transcripts via GraphQL queries to t.Log, and leaves
// the per-run DuckDB files on disk for manual review. Pass/fail is
// "did the runner crash"; behaviour judgment is by-eye.
//
// Entry point: TestScenarios in runner_test.go (one level up at
// tests/scenarios/runner_test.go) walks runs.yaml × *.yaml. Every
// scenario runs as a t.Run subtest so the standard go test
// `-run TestScenarios/<run>/<scenario>` selectors work.
//
// See design/001-agent-runtime/phase-4.1b-spec.md for the contract
// shape and ../agent/tests/scenarios/harness/ for the prior-art ADK
// harness this re-implements.
package harness
