// Package main is the python-mcp binary — phase 3.5 of the hugen
// agent runtime. It exposes two MCP tools (python.run_code,
// python.run_script) over stdio.
//
// Two CLI modes:
//
//	--create-template <requirements.txt> [--out <path>]
//	  Build mode (operator one-shot). Builds a relocatable venv via
//	  `uv venv --relocatable` + `uv pip install -r ...` and writes a
//	  <out>/.bootstrap-complete stamp. Idempotent re-run.
//
//	--template <path>
//	  Server mode. Runs as a per_agent stdio MCP server (one process
//	  for the whole runtime). On every tool call it reads session_id
//	  from `_meta.session_id`, computes <WORKSPACES_ROOT>/<sid>/.venv/,
//	  takes a fast-path os.Stat on the per-session bootstrap stamp,
//	  and either reuses the existing copy or lazily copies the
//	  template into the session workspace before spawning Python.
//	  No mutex / sync.Once / singleflight: mark3labs/mcp-go runs each
//	  CallTool in its own goroutine and LLMs emit sequential tool
//	  calls within a session.
//
// Each call forks a fresh <session_venv>/bin/python — no REPL state.
// Hugr credentials (HUGR_URL + HUGR_TOKEN, refreshed against
// HUGR_TOKEN_URL on the loopback) are exported into every spawned
// subprocess so hugr-client inside scripts can authenticate;
// refresh is between calls, not within one call.
//
// See specs/004-analyst-toolkit/contracts/python-mcp.md for the
// CLI, env, and tool envelope contract.
package main
