# Attach files as queryable tables

> Adapted from upstream
> [`duckdb/duckdb-skills/skills/attach-db/`](https://github.com/duckdb/duckdb-skills/tree/main/skills/attach-db)
> (MIT). Persistent state-file flow dropped — DuckDB here is `:memory:` per
> session and dies with `Workspace.Release`.

Attach a file as a named table when you query it more than once in the
same session. For one-shot reads, prefer the inline `read_parquet(...)` /
`read_csv_auto(...)` form documented in `query.md`.

## Two flavours

### A. ATTACH a `.duckdb` database file

Persistent files (created earlier by another session, or shipped as part
of a project's data) attach with `ATTACH`:

```sql
ATTACH 'data/sales.duckdb' AS sales (READ_ONLY);
USE sales;
SHOW TABLES;
```

`READ_ONLY` is the safe default — drop it only when you intend to
`CREATE TABLE` / `INSERT` against the attached file. Note that the file
lives in the session workspace; closing the session reclaims it unless
you `COPY ... TO 'data/...parquet'` it out first.

### B. CREATE VIEW over a file you'll reuse

For Parquet / CSV / JSON / GeoJSON, the cheapest "table" is a view:

```sql
CREATE OR REPLACE VIEW customers AS
  FROM 'data/customers.parquet';

CREATE OR REPLACE VIEW orders AS
  FROM 'data/orders/part-*.parquet';
```

Views are evaluated lazily and never copy bytes. They survive across
queries inside the same session and disappear at session close.

## Schema introspection

After attaching / creating a view, introspect with the dedicated tools —
they're cheaper than reading file headers:

| Goal                              | Call                                                |
|-----------------------------------|-----------------------------------------------------|
| What databases are loaded         | `duckdb-mcp:list_databases`                         |
| What tables are in a database     | `duckdb-mcp:list_tables` (`database?`, `schema?`)   |
| What columns does a table have    | `duckdb-mcp:list_columns` (`table`, `database?`)    |
| Same, inline                      | `DESCRIBE customers;` via `execute_query`           |
| Quick stats for a table           | `SUMMARIZE customers;` via `execute_query`          |

`SUMMARIZE` is a one-call statistical profile (min, max, avg, null %,
distinct cardinality) — useful before you write a real aggregation.

## Common patterns

### Glob multiple files into one logical table

```sql
CREATE OR REPLACE VIEW events AS
  FROM 'data/events/year=*/month=*/*.parquet';

SELECT count() FROM events;
```

DuckDB pushes predicates into the per-file Parquet readers, so
`WHERE event_date >= '2026-01-01'` is fast even on hundreds of files.

### Attach and copy: produce a `.duckdb` artefact

```sql
ATTACH 'data/snapshot.duckdb' AS out;
CREATE TABLE out.customers AS
  FROM 'data/customers.parquet';
DETACH out;
```

The resulting `.duckdb` file is portable (`out.customers` can be queried
by any DuckDB ≥ matching version).

### Attach failures

| Error                                            | Fix                                                                |
|--------------------------------------------------|--------------------------------------------------------------------|
| `Cannot open file ...`                           | Path is relative to the session cwd; use `bash.list_dir` to verify |
| `Database "X" already attached`                  | `DETACH X;` first or pick a fresh alias                            |
| `Catalog Error: ... Did you mean ...`            | Wrong table name — `list_tables` against the attached database     |
| `IO Error: Could not read parquet metadata`      | File is truncated or not Parquet — open via `read.md` macro first  |

## Don't carry over from upstream

- The `~/.duckdb-skills/state.sql` flow — there is no shared state file
  here. Each session is its own connection, lifecycle bound to the
  session workspace.
- `duckdb -init` invocations — we don't shell out to the CLI; every SQL
  statement is one `duckdb-mcp:execute_query` call.
- Database alias derivation by stripping path segments — pick a short
  semantic alias (e.g. `sales`, `events`) that the rest of the
  conversation can refer to.
