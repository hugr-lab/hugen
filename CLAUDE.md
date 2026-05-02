# hugen Development Guidelines

Auto-generated from all feature plans. Last updated: 2026-05-01

## Active Technologies
- Go 1.26.1 (from `go.mod`); host Python 3.x (operator-installed); host `uv` (operator-installed; `--relocatable` venv builder). (004-analyst-toolkit)

- Go 1.26.1 (from `go.mod`) (001-agent-runtime-phase-1, 002-agent-runtime-phase-2, 003-agent-runtime-phase-3)
- DuckDB via `pkg/store/local` (consumed through `pkg/runtime.RuntimeStore`); phase 2 reconnection replay queries `session_events` by `seq` ascending — no schema migration. (002-agent-runtime-phase-2)
- `net/http` + `embed.FS` for `pkg/adapter/http` (JSON+SSE) and `pkg/adapter/webui` (static UI). No third-party router, no JS bundler. (002-agent-runtime-phase-2)
- Three-tier permission stack `pkg/auth/perm`: Tier-1 LocalPermissions (config floor) + Tier-2 RemotePermissions (`function.core.auth.my_permissions` snapshot, TTL + singleflight + 3× hard expiry) + Tier-3 `pkg/tool.Policies` against `tool_policies` table. (003-agent-runtime-phase-3)
- `mark3labs/mcp-go` for MCP transport (server in `mcp/bash-mcp` + `mcp/hugr-query`, client in `pkg/tool.MCPProvider`); `apache/arrow-go/v18` + `pqarrow` for the hugr-query Parquet writer. (003-agent-runtime-phase-3)

## Project Structure

```text
src/
tests/
```

## Commands

# Add commands for Go 1.26.1 (from `go.mod`)

## Code Style

Go 1.26.1 (from `go.mod`): Follow standard conventions

## Recent Changes
- 004-analyst-toolkit: Analyst toolkit (phase 3.5) — vendored `motherduckdb/mcp-server-motherduck` v1.0.6 (MIT) under `vendor/` (per_session, uvx-spawned, `--init-sql` hardening); new `mcp/python-mcp/` Go binary (~250 LOC; `--create-template` builds relocatable venv via `uv venv --relocatable`, `--template` runs as stdio MCP with `sync.Once` lazy bootstrap + CoW copy + `.bootstrap-complete` stamp; per-call HUGR token refresh via existing `*hugr.RemoteStore`, injected as `HUGR_TOKEN`); `system:skill_files` tool in `pkg/tool/system.go`; bundled skills `assets/skills/{duckdb-data,duckdb-docs,python-runner}/` (DuckDB skills adapted from `duckdb/duckdb-skills` MIT, Hugr Python client ref ported from `hugr-lab.github.io/docs/6-querying/4-python-client.md`); `assets/python/requirements.txt` (pandas, pyarrow, duckdb, hugr-client, matplotlib, plotly, great_tables, folium, weasyprint); host prereqs: Python ≥ 3.10, uv ≥ 0.4.0, Cairo/Pango/gdk-pixbuf/libffi for weasyprint; `cmd/hugen/sessions.go: spawnBashMCP` refactored to generic `spawnPerSessionMCP(name)`; no new DB schema, no new third-party Go deps; ADK still quarantined.

- 003-agent-runtime-phase-3: Action layer — `pkg/config` + `pkg/skill` + `pkg/tool` + `pkg/auth/perm` + `pkg/auth/template`; new binaries `mcp/bash-mcp` and `mcp/hugr-query`; bundled skill `assets/skills/hugr-data/`; `tool_policies` table (Tier 3) with `policy_save`/`policy_revoke` system tools; Tier-2 RemotePermissions with TTL refresh + `runtime_reload`; no-Hugr (US5) deployment via `skill.AnnotateUnavailable` + existing config guards; ADK still quarantined.
- 002-agent-runtime-phase-2: HTTP/SSE adapter, web UI, ADK eviction; `cmd/hugen/runtime.go` (RuntimeCore) splits bootstrap from adapter wiring.

<!-- MANUAL ADDITIONS START -->
<!-- MANUAL ADDITIONS END -->
