# Ad-hoc SQL queries

> Adapted from upstream
> [`duckdb/duckdb-skills/skills/query/`](https://github.com/duckdb/duckdb-skills/tree/main/skills/query)
> (MIT). Sandbox / state / `duckdb -init` flow dropped — every statement
> is one `duckdb-mcp:execute_query` call against the per-session
> `:memory:` connection. The "Friendly SQL" idioms appendix is preserved
> verbatim because that's the load-bearing part for SQL composition.

## When to read this

The first time you compose SQL in the session. The Friendly-SQL appendix
covers ~80 % of the patterns that fit one statement; the rest live in
`duckdb-docs`.

## One statement, one call

`duckdb-mcp:execute_query` accepts a single SQL statement (or a
semicolon-separated batch). Multi-statement batches share the
session-level `:memory:` connection, so you can `CREATE VIEW ...;
SELECT ...;` in one call:

```sql
CREATE OR REPLACE VIEW customers AS
  FROM 'data/customers.parquet';
SELECT count() AS n FROM customers;
```

## Estimate result size before running open queries

Before running an unbounded `SELECT * FROM huge_file` ask yourself: how
many rows will land in the model context?

| Quick check                | SQL                                                       |
|----------------------------|-----------------------------------------------------------|
| Row count of a file        | `SELECT count() FROM 'data/X.parquet'`                    |
| Schema only, no rows       | `DESCRIBE FROM 'data/X.parquet'`                          |
| First N rows (preview)     | `FROM 'data/X.parquet' LIMIT 20`                          |
| Stat profile               | `SUMMARIZE FROM 'data/X.parquet'`                         |
| Parquet metadata only      | `SELECT * FROM parquet_metadata('data/X.parquet')`        |

If a query would return more than a few hundred rows of full content,
write the result to a file with `COPY` (see `convert.md`) and pivot to
aggregations or `LIMIT`.

## Direct file queries

DuckDB reads Parquet, CSV, JSON, and a few others as if they were tables:

```sql
FROM 'data/sales.parquet';
FROM 'data/sales.csv';
FROM 'data/events/*.parquet';
```

For JSON and CSV, the explicit `read_*` form lets you tune parsing:

```sql
FROM read_csv('data/x.csv', header = true, delim = '|', sample_size = -1);
FROM read_json_auto('data/x.json', maximum_object_size = 16777216);
```

## Errors and recovery

| Symptom                                 | Action                                                                                       |
|-----------------------------------------|----------------------------------------------------------------------------------------------|
| Syntax error                            | Read the `duckdb-docs` skill or the matching reference here; do NOT retry the same query     |
| `Catalog Error: Table "X" does not exist` | `list_tables` to enumerate; views/tables die with the session — recreate them                |
| `Conversion Error: Could not convert string '...' to ...` | Force a wider type or use `TRY_CAST(col AS DOUBLE)`                                          |
| `Out of Memory Error`                   | Add a `WHERE`, narrow `SELECT`, or stream via `COPY ... TO ...`                              |
| `IO Error: ...`                         | Path or permissions — verify with `bash.list_dir`                                            |

---

## DuckDB Friendly SQL Reference

Verbatim from upstream `query` SKILL — the highest-leverage idioms for
DuckDB SQL composition. Prefer these over the strict-SQL alternatives.

### Compact clauses

- **FROM-first**: `FROM table WHERE x > 10` (implicit `SELECT *`)
- **GROUP BY ALL**: auto-groups by all non-aggregate columns
- **ORDER BY ALL**: orders by all columns for deterministic results
- **SELECT * EXCLUDE (col1, col2)**: drop columns from wildcard
- **SELECT * REPLACE (expr AS col)**: transform a column in-place
- **UNION ALL BY NAME**: combine tables with different column orders
- **Percentage LIMIT**: `LIMIT 10%` returns a percentage of rows
- **Prefix aliases**: `SELECT x: 42` instead of `SELECT 42 AS x`
- **Trailing commas** allowed in SELECT lists

### Query features

- **count()**: no need for `count(*)`
- **Reusable aliases**: use column aliases in WHERE / GROUP BY / HAVING
- **Lateral column aliases**: `SELECT i+1 AS j, j+2 AS k`
- **COLUMNS(*)**: apply expressions across columns; supports regex,
  EXCLUDE, REPLACE, lambdas
- **FILTER clause**: `count() FILTER (WHERE x > 10)` for conditional
  aggregation
- **GROUPING SETS / CUBE / ROLLUP**: advanced multi-level aggregation
- **Top-N per group**: `max(col, 3)` returns top 3 as a list; also
  `arg_max(arg, val, n)`, `min_by(arg, val, n)`
- **DESCRIBE table_name**: schema summary
- **SUMMARIZE table_name**: instant statistical profile
- **PIVOT / UNPIVOT**: reshape between wide and long formats
- **SET VARIABLE x = expr**: SQL-level variables; reference with
  `getvariable('x')`

### Data import

- **Direct file queries**: `FROM 'file.csv'`, `FROM 'data.parquet'`
- **Globbing**: `FROM 'data/part-*.parquet'`
- **Auto-detection**: CSV headers and schemas inferred automatically

### Expressions and types

- **Dot operator chaining**: `'hello'.upper()` or `col.trim().lower()`
- **List comprehensions**: `[x*2 FOR x IN list_col]`
- **List/string slicing**: `col[1:3]`, negative indexing `col[-1]`
- **STRUCT.* notation**: `SELECT s.* FROM (SELECT {'a': 1, 'b': 2} AS s)`
- **Square bracket lists**: `[1, 2, 3]`
- **format()**: `format('{}->{}', a, b)` for string formatting

### Joins

- **ASOF joins**: approximate matching on ordered data (timestamps)
- **POSITIONAL joins**: match rows by position, not keys
- **LATERAL joins**: reference prior table expressions in subqueries

### Data modification

- **CREATE OR REPLACE TABLE**: no need for `DROP TABLE IF EXISTS`
- **CREATE TABLE ... AS SELECT (CTAS)**: create tables from query results
- **INSERT INTO ... BY NAME**: match columns by name, not position
- **INSERT OR IGNORE INTO / INSERT OR REPLACE INTO**: upsert patterns
