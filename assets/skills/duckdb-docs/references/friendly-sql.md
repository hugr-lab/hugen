# DuckDB Friendly SQL

> Sourced from the official DuckDB docs and upstream
> [`duckdb/duckdb-skills`](https://github.com/duckdb/duckdb-skills) (MIT).
> Canonical reference for DuckDB-specific syntax. Read this before
> falling back to "standard SQL" patterns — DuckDB's idioms are usually
> shorter and more correct.

## Compact clauses

- **FROM-first**: `FROM table WHERE x > 10` (implicit `SELECT *`)
- **GROUP BY ALL**: auto-groups by every non-aggregate column
- **ORDER BY ALL**: orders by every column for deterministic output
- **SELECT * EXCLUDE (col1, col2)**: drop columns from a wildcard
- **SELECT * REPLACE (expr AS col)**: transform a column in place
- **UNION ALL BY NAME**: combine tables with different column orders
- **Percentage LIMIT**: `LIMIT 10%` returns a fraction of rows
- **Prefix aliases**: `SELECT x: 42` is `SELECT 42 AS x`
- **Trailing commas** are allowed in SELECT lists

## Query features

- **`count()`** — no `*` argument needed; counts rows
- **Reusable aliases** — `SELECT x AS y FROM t WHERE y > 0` works
- **Lateral aliases** — `SELECT i+1 AS j, j+2 AS k`
- **`COLUMNS(*)`** — apply an expression across columns; supports
  regex, `EXCLUDE`, `REPLACE`, lambdas
- **`FILTER` clause** — `count() FILTER (WHERE x > 10)` for conditional
  aggregation
- **GROUPING SETS / CUBE / ROLLUP** — multi-level aggregation
- **Top-N per group** — `max(col, 3)` returns the top 3 as a list;
  `arg_max(arg, val, n)`, `min_by(arg, val, n)`
- **`DESCRIBE table`** — schema summary
- **`SUMMARIZE table`** — instant statistical profile
- **`PIVOT` / `UNPIVOT`** — reshape between wide and long
- **`SET VARIABLE x = expr`** — SQL-level variables; reference with
  `getvariable('x')`

## Data import

- **Direct file queries** — `FROM 'file.csv'`, `FROM 'data.parquet'`
- **Globbing** — `FROM 'data/part-*.parquet'`
- **Auto-detection** — CSV headers and schemas are inferred

## Expressions and types

- **Dot operator chaining** — `'hello'.upper()`, `col.trim().lower()`
- **List comprehensions** — `[x*2 FOR x IN list_col]`
- **List / string slicing** — `col[1:3]`, negative `col[-1]`
- **`STRUCT.*` notation** — `SELECT s.* FROM (SELECT {'a':1,'b':2} s)`
- **Square-bracket lists** — `[1, 2, 3]`
- **`format()`** — `format('{}->{}', a, b)`

## Joins

- **ASOF joins** — approximate matching on ordered data (timestamps)
- **POSITIONAL joins** — match rows by position, not key
- **LATERAL joins** — reference prior tables in subqueries

## Data modification

- **`CREATE OR REPLACE TABLE`** — no need for `DROP TABLE IF EXISTS`
- **CTAS** — `CREATE TABLE x AS SELECT ...`
- **`INSERT INTO ... BY NAME`** — match columns by name, not position
- **`INSERT OR IGNORE` / `INSERT OR REPLACE`** — upsert patterns
