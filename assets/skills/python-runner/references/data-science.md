# Data science workflow (pandas, pyarrow, duckdb)

The bundled venv carries pandas + pyarrow + duckdb. The right tool depends
on the input size and the shape of the question.

| Workload | Right tool |
|----------|------------|
| Tabular file < 5M rows, complex per-row transform | `pandas` |
| Big columnar files, projection / filter / aggregate | `duckdb` (in-process SQL on Parquet/CSV) |
| Reading Arrow / sharing data zero-copy with `hugr-client` | `pyarrow` |
| Joins / window functions across multiple files | `duckdb` |
| Anything you'd write SQL for — write SQL via `duckdb` | `duckdb` |

## Reading inputs

Inputs typically land under `${SESSION_DIR}/data/` — produced earlier by
`hugr-query` (Parquet) or by an upstream `bash.write_file` (CSV).

```python
import os, pandas as pd, pyarrow.parquet as pq
DATA = os.path.join(os.environ["SESSION_DIR"], "data")

df = pd.read_parquet(os.path.join(DATA, "customers.parquet"))
# zero-copy alternative:
table = pq.read_table(os.path.join(DATA, "customers.parquet"))
```

## DuckDB — SQL on files

DuckDB reads Parquet/CSV/JSON without loading the file into memory first.
Prefer it for filter+aggregate over large files.

```python
import duckdb, os
DATA = os.path.join(os.environ["SESSION_DIR"], "data")

con = duckdb.connect()  # in-memory; new connection per spawn
df = con.execute(f"""
    SELECT region, COUNT(*) AS n, SUM(amount) AS total
    FROM read_parquet('{DATA}/sales.parquet')
    WHERE order_ts >= '2025-01-01'
    GROUP BY region
    ORDER BY total DESC
""").df()
```

Read multiple files with a glob — DuckDB stitches them as one table:

```python
con.execute(f"SELECT * FROM read_parquet('{DATA}/sales/*.parquet') LIMIT 10").df()
```

Register a pandas DataFrame so you can join SQL ↔ Python:

```python
con.register("members", members_df)
joined = con.execute("""
    SELECT s.*, m.tier
    FROM read_parquet('sales.parquet') s
    LEFT JOIN members m ON m.id = s.member_id
""").df()
```

The DuckDB MCP (`duckdb-mcp`, separate skill) covers the long-running
analyst flow with persistent state. Use the in-process `duckdb` Python
package for one-shot transforms — no shared state with the MCP.

## Pandas patterns

### Schema + first rows
```python
print(df.dtypes)
print(df.head().to_string())
print(df.describe(include="all").to_string())
```

### Group-by aggregate
```python
out = (
    df.groupby(["region", "channel"], as_index=False)
      .agg(orders=("id", "size"), total=("amount", "sum"))
      .sort_values("total", ascending=False)
)
```

### Window function (rolling / cumulative)
```python
df["amount_cum"] = df.sort_values("order_ts").groupby("customer_id")["amount"].cumsum()
```

### Date parsing — always with explicit timezone
```python
df["order_ts"] = pd.to_datetime(df["order_ts"], utc=True)
```

Dates from Hugr arrive as TIMESTAMPTZ (UTC) — read them with
`utc=True` and convert in display, never the other way round.

## Writing outputs

Always under `${SESSION_DIR}/data/` for later steps in the same session.

```python
out_path = os.path.join(os.environ["SESSION_DIR"], "data/customers_clean.parquet")
df.to_parquet(out_path, index=False, compression="snappy")
```

Parquet is the default — round-trips through DuckDB and Hugr cheaply.
CSV only when the consumer explicitly needs it (export to a non-Hugr
tool):

```python
df.to_csv(os.path.join(os.environ["SESSION_DIR"], "data/customers.csv"), index=False)
```

## Memory hygiene

- Drop columns you don't need before aggregating:
  `df = df[["customer_id", "amount"]]` first, group-by second.
- For >1M rows, prefer DuckDB SQL — `pandas` group-by builds intermediate
  copies; DuckDB streams and is 5–20× faster + leaner.
- Don't keep multiple snapshots: reuse the same name (`df = df.assign(...)`)
  rather than `df1 → df2 → df3`.

## Common errors

- **`UnicodeDecodeError` reading CSV** — use `pd.read_csv(..., encoding="utf-8-sig")`
  to strip the BOM Excel writes.
- **Mixed-type columns: `Object` everywhere** — the source CSV had inconsistent
  values. Read with `dtype={"col": str}` and clean explicitly.
- **`OutOfMemory` on group-by** — switch to DuckDB SQL, see above.
- **`KeyError` on a path lookup from `hugr-client`** — the part path is wrong.
  `result.parts.keys()` lists the actual paths; the prefix is `data.` for
  query results, deeper for nested modules.
