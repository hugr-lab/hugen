# Inline file reads via `read_any`

> Adapted from upstream
> [`duckdb/duckdb-skills/skills/read-file/`](https://github.com/duckdb/duckdb-skills/tree/main/skills/read-file)
> (MIT). Bash wrapper dropped — the macro is registered inside the
> per-session `:memory:` connection via `duckdb-mcp:execute_query` and
> reused for the rest of the session.

## When to read this

The first time a user asks "what's in this file" or hands you a file
path of an unknown format. The `read_any` macro dispatches by extension
and returns a unified table view.

## Register the macro once per session

Run this inside `duckdb-mcp:execute_query`:

```sql
CREATE OR REPLACE MACRO read_any(file_name) AS TABLE
  WITH json_case     AS (FROM read_json_auto(file_name)),
       csv_case      AS (FROM read_csv(file_name)),
       parquet_case  AS (FROM read_parquet(file_name)),
       avro_case     AS (FROM read_avro(file_name)),
       blob_case     AS (FROM read_blob(file_name)),
       spatial_case  AS (FROM st_read(file_name)),
       excel_case    AS (FROM read_xlsx(file_name)),
       sqlite_case   AS (
         FROM sqlite_scan(
           file_name,
           (SELECT name FROM sqlite_master(file_name) LIMIT 1)
         )
       ),
       ipynb_case AS (
         WITH nb AS (FROM read_json_auto(file_name))
         SELECT cell_idx, cell.cell_type,
                array_to_string(cell.source, '') AS source,
                cell.execution_count
         FROM nb, UNNEST(cells) WITH ORDINALITY AS t(cell, cell_idx)
         ORDER BY cell_idx
       )
  FROM query_table(
    CASE
      WHEN file_name ILIKE '%.json'  OR file_name ILIKE '%.jsonl'
        OR file_name ILIKE '%.ndjson' OR file_name ILIKE '%.geojson'
        OR file_name ILIKE '%.geojsonl' OR file_name ILIKE '%.har' THEN 'json_case'
      WHEN file_name ILIKE '%.csv' OR file_name ILIKE '%.tsv'
        OR file_name ILIKE '%.tab' OR file_name ILIKE '%.txt' THEN 'csv_case'
      WHEN file_name ILIKE '%.parquet' OR file_name ILIKE '%.pq'  THEN 'parquet_case'
      WHEN file_name ILIKE '%.avro'                                THEN 'avro_case'
      WHEN file_name ILIKE '%.xlsx' OR file_name ILIKE '%.xls'    THEN 'excel_case'
      WHEN file_name ILIKE '%.shp'  OR file_name ILIKE '%.gpkg'
        OR file_name ILIKE '%.fgb'  OR file_name ILIKE '%.kml'    THEN 'spatial_case'
      WHEN file_name ILIKE '%.ipynb'                               THEN 'ipynb_case'
      WHEN file_name ILIKE '%.db' OR file_name ILIKE '%.sqlite'
        OR file_name ILIKE '%.sqlite3'                             THEN 'sqlite_case'
      ELSE 'blob_case'
    END
  );
```

It survives until the session closes; you can re-run it without harm
(`CREATE OR REPLACE`).

## Three-step preview

After registering, profile any file with three small statements:

```sql
DESCRIBE FROM read_any('data/customers.csv');
SELECT count(*) AS row_count FROM read_any('data/customers.csv');
FROM read_any('data/customers.csv') LIMIT 20;
```

Run them in one `execute_query` call — the schema, count, and head
fit in a single round-trip.

## When the macro misclassifies

The dispatch is filename-driven. For files with non-canonical extensions
or no extension, call the explicit reader directly:

| Reader              | Use for                                                   |
|---------------------|-----------------------------------------------------------|
| `read_csv(...)`     | tunable CSV (delimiters, sample size, schema overrides)   |
| `read_csv_auto(...)`| simpler API; auto-detect everything                       |
| `read_parquet(...)` | force Parquet on an unconventional extension              |
| `read_json_auto(...)` | newline-delimited or array JSON                         |
| `st_read(...)`      | any GDAL-supported spatial format (also see `spatial.md`) |
| `sqlite_scan(...)`  | SQLite tables (`sqlite_scanner` extension)                |

## Excel and SQLite

`read_xlsx` requires the `excel` extension; `sqlite_scan` requires
`sqlite_scanner`. Neither is pre-loaded — pay the one-call install cost
the first time:

```sql
INSTALL excel;          LOAD excel;
INSTALL sqlite_scanner; LOAD sqlite_scanner;
```

Subsequent calls in the session reuse the cached binary.

## Reading remote URLs

`https://` and `s3://` URLs work as the `file_name` argument when
`httpfs` is loaded (it is, by default). For credentials see `s3.md`.
