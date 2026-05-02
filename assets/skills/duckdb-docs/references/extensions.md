# DuckDB extensions — quick reference

> Canonical list at <https://duckdb.org/docs/extensions/overview>.

DuckDB ships a small core; everything below loads on demand. In this
runtime the operator pre-loads `httpfs` and `spatial` via `--init-sql`
on the per-session DuckDB MCP — skill content does NOT call `INSTALL`
for those two.

## What's pre-loaded

| Extension | What it adds                                                 |
|-----------|--------------------------------------------------------------|
| `httpfs`  | `http://`, `https://`, `s3://`, `gcs://`, `azure://` reads  |
| `spatial` | GEOMETRY type + ST_* functions + `st_read` + GDAL writers    |

## Common opt-in extensions

Pay one `INSTALL <name>; LOAD <name>;` round-trip the first time a
session needs the extension; the binary is cached under
`~/.duckdb/extensions/` and re-used on subsequent sessions.

| Extension            | What it adds                                                        |
|----------------------|----------------------------------------------------------------------|
| `excel`              | `read_xlsx(...)`, `(FORMAT xlsx)` writes                             |
| `sqlite_scanner`     | `sqlite_scan(file, table)` to query `.sqlite` / `.db` files          |
| `postgres_scanner`   | `postgres_scan(connstr, schema, table)` for live Postgres reads      |
| `mysql_scanner`      | Same idea for MySQL                                                  |
| `iceberg`            | `iceberg_scan(...)` to read Apache Iceberg tables                    |
| `delta`              | `delta_scan(...)` to read Delta Lake tables                          |
| `parquet`            | Always loaded by default                                              |
| `json`               | Always loaded by default                                              |
| `fts`                | Full-text search (BM25); `match_bm25(...)`                            |
| `vss`                | Vector similarity search                                              |
| `azure`              | Azure Storage native auth                                             |

## Community extensions

Loaded with the explicit `FROM community` qualifier:

```sql
INSTALL h3 FROM community;
LOAD h3;
```

Common picks: `h3` (hexagonal binning), `prql` (PRQL → SQL),
`tabletools` (extended SUMMARIZE / DESCRIBE), `crypto` (hashes).

Browse: <https://duckdb.org/community_extensions>.

## Inspecting what's loaded

```sql
SELECT extension_name, loaded, installed
FROM duckdb_extensions()
ORDER BY extension_name;
```

## Secrets (httpfs / cloud)

Each cloud extension uses `CREATE SECRET (TYPE ..., PROVIDER ...)`. See
the workflow skill `duckdb-data:s3` for the canonical setup matrix.

## Configuration knobs

```sql
SET memory_limit = '4GB';
SET threads = 4;
SET temp_directory = '.duckdb/tmp';
SET secret_directory = '.duckdb/secrets';
SET enable_progress_bar = false;     -- non-interactive sessions
SET errors_as_json = true;           -- structured error envelopes
```

Read with `SELECT current_setting('memory_limit')` or
`SELECT * FROM duckdb_settings()`.

In this runtime, `memory_limit` / `threads` / `temp_directory` /
`secret_directory` are set by the operator via the duckdb-mcp
`--init-sql` flag. Skills can override per-session if needed; a
`reset` then restores the operator default.
