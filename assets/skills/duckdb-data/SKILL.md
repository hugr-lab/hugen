---
name: duckdb-data
license: MIT
description: >
  Run analytical SQL on workspace files via DuckDB (in-memory, per
  session). The cheapest path for joining and post-processing data
  pulled from upstream sources (Hugr, REST exports, hand-written CSVs)
  — attach Parquet / CSV / JSON / GeoJSON as queryable tables,
  cross-file joins, aggregations and window functions, format
  conversion via COPY, queries against S3 / HTTP URLs, spatial ops
  (ST_Distance, ST_Intersects). For DuckDB syntax / function lookup
  load `duckdb-docs` alongside; for fetching source data use
  `hugr-data`; for charts / HTML / PDF reports use `python-runner`.
allowed-tools:
  - provider: duckdb-mcp
    tools:
      - execute_query
      - list_databases
      - list_tables
      - list_columns
metadata:
  hugen:
    requires: [_system]
    autoload: false
    autoload_for: []
    sub_agents: []
    intents: [data_query, file_analysis, spatial_analysis]
compatibility:
  model: any
  runtime: hugen-phase-3.5
---

# DuckDB Data

Per-session in-memory DuckDB with `spatial` and `httpfs` pre-loaded by the
operator's `--init-sql`. The connection is born when the session opens and
dies on close — no on-disk database file, no cross-session leakage. Spill
files and secrets live under `${SESSION_DIR}/.duckdb/` and are reaped with
the rest of the workspace.

> Adapted from upstream [`duckdb/duckdb-skills`](https://github.com/duckdb/duckdb-skills)
> (MIT). Control flow rewritten from `Bash + duckdb` CLI to
> `duckdb-mcp:execute_query`; persistent-store and per-call `INSTALL`
> content dropped (the operator's startup hook handles `spatial` /
> `httpfs`).

## Four tools

- **`duckdb-mcp:execute_query`** — run any SQL statement. Args: `sql`
  (string). Returns a JSON envelope with rows + metadata.
- **`duckdb-mcp:list_databases`** — enumerate attached and known
  databases. No args.
- **`duckdb-mcp:list_tables`** — enumerate tables / views in a database.
  Args: `database?`, `schema?`.
- **`duckdb-mcp:list_columns`** — column metadata for one table.
  Args: `table` (required), `database?`, `schema?`.

## Standard workflow

NEVER guess a function or operator before consulting `duckdb-docs` — DuckDB
SQL diverges from Postgres in places (FILTER clause, QUALIFY, list / struct
/ map types, named-parameter calls).

1. **Use relative paths** — files live under `${SESSION_DIR}/`. DuckDB
   resolves them against its cwd, which the runtime sets to that dir.
2. **Compose SQL** — point `read_parquet('data/X.parquet')` /
   `read_csv_auto('data/X.csv')` at the file directly. No intermediate
   `ATTACH` is needed for one-off reads.
3. **Write outputs via COPY** — `COPY (SELECT ...) TO 'data/out.parquet'`.
   Surface relative paths back to the user.

## Critical rules

- **Relative paths only** — `read_parquet('data/X.parquet')`, not
  `/abs/...`. The session workspace is the cwd; absolute paths leak the
  host's view of disk.
- **Do NOT call `INSTALL`** for `spatial` or `httpfs` — they are loaded by
  the operator's `--init-sql`. Per-call `INSTALL` re-downloads from the
  extension repository on every session and is forbidden by the skill.
- **In-memory only** — `:memory:` per session. Persistent state belongs
  in files (`COPY ... TO 'data/X.parquet'`) or upstream stores (Hugr).
  `read-memories` style flows from upstream are NOT carried over.
- **`switch_database_connection` is not exposed** to the LLM — opening
  another `.duckdb` file is an operator decision, not a skill one. If you
  need a separate database, `ATTACH 'path' AS name` works inside
  `execute_query`.
- **Surface relative paths to the user** — they read files via the
  workspace root, not the host root.

## Why not Python first?

DuckDB SQL is the cheapest path for: schema introspection, row counts,
single-pass aggregations, joins across files, format conversions,
geospatial primitives, and S3 reads. Python (`python-runner`) is the
right tool when you need: charts, HTML / PDF reports, multi-step
transformations that don't fit one SELECT, or libraries DuckDB doesn't
expose. SQL first, Python second.
