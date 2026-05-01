// Package main is the python-mcp binary — phase 3.5 of the hugen
// agent runtime. It owns the per-session Python virtualenv lifecycle
// and exposes two MCP tools (python.run_code, python.run_script) over
// stdio. Operators run it once in build mode (--create-template
// <requirements.txt>) to produce a relocatable venv at install time;
// the runtime spawns it per session in server mode (--template
// <path>), which lazily copies the template into <session>/.venv on
// the first tool call (sync.Once-coalesced, CoW where supported, with
// a .bootstrap-complete stamp for crash recovery). Each call forks a
// fresh .venv/bin/python — there is no REPL state. Hugr credentials
// (HUGR_URL + HUGR_TOKEN, refreshed against HUGR_TOKEN_URL on the
// loopback) are exported into every spawned subprocess so hugr-client
// inside scripts can authenticate; refresh is per-call, not in-call.
// See specs/004-analyst-toolkit/contracts/python-mcp.md for the full
// CLI, env, and tool envelope contract.
package main
