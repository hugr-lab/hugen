# Workspace conventions

> Fresh content authored for the hugen runtime — no upstream
> equivalent. Covers how DuckDB SQL fits into the per-session workspace,
> how it bridges to `hugr-data` / `hugr-query` outputs, and the path
> conventions every analyst skill in this repo follows.

## When to read this

Always — first thing in any session that loads `duckdb-data`. The
patterns below are what makes a multi-step analyst flow work without
absolute paths leaking the host's view of disk.

## Session layout

DuckDB is spawned with cwd set to `${SESSION_DIR}` — the per-session
workspace dir. Every relative path in SQL resolves against that root.
The runtime cleans this dir on session close.

```text
${SESSION_DIR}/
├── data/               # input + intermediate data files
│   └── *.parquet, *.csv, *.json, *.geojson
├── reports/            # HTML / PDF reports (python-runner)
├── figures/            # static images (python-runner)
└── .duckdb/
    ├── tmp/            # spill files
    └── secrets/        # session-scoped DuckDB secrets
```

`bash-mcp`, `hugr-query`, `python-mcp`, and `duckdb-mcp` all share this
cwd — a Parquet file written by `hugr-query` lands at the same path
DuckDB and Python see.

## Three-leg analyst loop

The canonical flow uses each provider for what it's best at:

```text
hugr-query.hugr.Query   →   data/*.parquet         (provided by hugr-data)
duckdb-mcp:execute_query →  joins / aggregates / format conversions
python-mcp:run_script    →  charts, HTML / PDF reports
```

Example end-to-end (each step is one tool call):

1. `hugr-data` skill → `hugr.Query(... result_url='data/sales.parquet')`
   writes the Parquet (see hugr-data skill for query syntax).
2. `duckdb-mcp:execute_query`:
   ```sql
   SELECT region, sum(amount) AS total
   FROM read_parquet('data/sales.parquet')
   GROUP BY 1 ORDER BY total DESC;
   ```
3. Need a chart or report? Hand off to `python-runner` — DuckDB SQL
   isn't the right tool for visualisation.

## Reading data the agent already has

Files written by `hugr-query`, `bash.write_file`, or earlier
`COPY ... TO 'data/...'` calls all live under `${SESSION_DIR}/data/` (or
wherever the writer put them). Address them with relative paths:

```sql
SELECT count() FROM read_parquet('data/customers.parquet');
SELECT * FROM read_csv_auto('data/manual_lookup.csv') LIMIT 5;
```

The session is the unit of state; nothing carries over to the next
session unless the user explicitly stores it (e.g. via `COPY ... TO`
into a path the operator persists).

## Output path conventions

| What you produce                          | Put it under         |
|-------------------------------------------|----------------------|
| Intermediate data file (Parquet, CSV, …)  | `data/`              |
| GeoJSON / GeoPackage you'll send to a map | `data/` or `reports/` (export-style) |
| `.duckdb` artefact for the user           | `data/`              |
| Anything an operator might look at later  | `reports/`           |

Surface the **relative** path back to the user (`./data/april.parquet`),
not the absolute path inside the session — the host's view of the
workspace differs and absolute paths from the session are
non-portable.

## Boundaries with sibling skills

| You want to…                                           | Use…              |
|--------------------------------------------------------|-------------------|
| Fetch fresh data from the platform                     | `hugr-data`       |
| Query / aggregate / convert files on disk              | `duckdb-data` (this skill) |
| Look up DuckDB function syntax without running it      | `duckdb-docs`     |
| Plot / map / build a report                            | `python-runner`   |
| Custom transformation that doesn't fit one SELECT      | `python-runner`   |

If the user asks "give me a chart" while only `duckdb-data` is loaded,
load `python-runner` (`/skill load python-runner`) — DuckDB does not
render charts. Conversely, if they ask "how many rows are in this
file" and only `python-runner` is loaded, the cheapest path is to load
`duckdb-data` and run a single `SELECT count()`.

## Common pitfalls

- **Absolute paths in SQL.** `read_parquet('/Users/.../data/x.parquet')`
  works but leaks the host root and breaks portability of the
  conversation transcript. Always relative.
- **Cross-session reads.** A path that worked in a previous session is
  gone — the workspace is reaped on close. If the user expects
  persistence, ask the operator to mount that directory under
  `HUGEN_WORKSPACE_DIR` with a stable name.
- **Mixing in-memory and on-disk DuckDB.** The session connection is
  `:memory:`; views / tables you create live there. To persist, write
  to a file (`COPY ... TO`) or attach an explicit `.duckdb` file
  (`ATTACH 'data/sink.duckdb' AS s`).
- **Forgetting `geometry_always_xy = true`.** Re-read `spatial.md` if
  any `ST_Distance_Spheroid` returns a strange number.
