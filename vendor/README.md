# `vendor/`

Third-party MCP servers vendored into hugen as git submodules. **Not** a `go mod vendor` directory.

## Pin policy

Each submodule is pinned to a specific upstream tag. The hugen build
refuses to compile against a divergent vendored tree (`make
submodule-check` runs as part of `make check` and exits non-zero on
any `+`/`-`/`U` prefix from `git submodule status --recursive`).

Updating a pin is a deliberate two-step operation:

```bash
cd vendor/<submodule>
git fetch --tags
git checkout <new-tag>
cd ../..
git add vendor/<submodule>
git commit -m "vendor: bump <submodule> to <new-tag>"
```

Patches against vendored code are forbidden in-tree. Bug fixes and
features go upstream (PR to the submodule repo); the next pin bump
brings them in.

## Current submodules

| Path | Upstream | Pinned tag | License | Phase |
|---|---|---|---|---|
| `mcp-server-motherduck/` | https://github.com/motherduckdb/mcp-server-motherduck | `v1.0.6` | MIT | 3.5 |

## `mcp-server-motherduck` — phase 3.5 analyst toolkit

Spawned per session by hugen as `uvx --from
./vendor/mcp-server-motherduck mcp-server-motherduck <flags>`. Exposes
the analytical SQL surface (DuckDB) the `duckdb-data` skill drives.
See `specs/004-analyst-toolkit/contracts/duckdb-mcp.md` for the
consumed tool surface and CLI flags, and
`specs/004-analyst-toolkit/research.md §R1–R2` for the rationale.

The `uv.lock` and Python source under this submodule are upstream
content — do not edit directly. Adjust hugen-side behaviour through
the `tool_providers:` config entry (`--init-sql`, `--read-write`, etc.).
