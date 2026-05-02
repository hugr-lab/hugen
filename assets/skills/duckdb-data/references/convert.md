# Format conversion via COPY

> Adapted from upstream
> [`duckdb/duckdb-skills/skills/convert-file/`](https://github.com/duckdb/duckdb-skills/tree/main/skills/convert-file)
> (MIT). Bash dispatch dropped — every conversion is one
> `duckdb-mcp:execute_query` call. The format-clause table is preserved
> verbatim because that's the load-bearing part.

## When to read this

Any time the user asks to "convert", "save as", "export to", or "turn
into" a file format. Output paths are relative to the session
workspace — keep them under `data/` for inputs/outputs and `reports/` /
`figures/` for visual artefacts.

## Format-clause table

| Output extension     | `COPY ... TO ...` clause                                | Extra setup                                |
|----------------------|---------------------------------------------------------|--------------------------------------------|
| `.parquet`, `.pq`    | *(no clause needed; default)*                            | —                                          |
| `.csv`               | `(FORMAT csv, HEADER)`                                   | —                                          |
| `.tsv`               | `(FORMAT csv, HEADER, DELIMITER '\t')`                   | —                                          |
| `.json`              | `(FORMAT json, ARRAY true)`                              | —                                          |
| `.jsonl`, `.ndjson`  | `(FORMAT json, ARRAY false)`                             | —                                          |
| `.xlsx`              | `(FORMAT xlsx)`                                          | `INSTALL excel; LOAD excel;` per-call      |
| `.geojson`           | `(FORMAT GDAL, DRIVER 'GeoJSON')`                        | `spatial` is pre-loaded                    |
| `.gpkg`              | `(FORMAT GDAL, DRIVER 'GPKG')`                           | `spatial` is pre-loaded                    |
| `.shp`               | `(FORMAT GDAL, DRIVER 'ESRI Shapefile')`                 | `spatial` is pre-loaded                    |

`spatial` and `httpfs` are pre-loaded by the operator's `--init-sql`;
`excel` is not, so the few `xlsx` flows pay one `INSTALL excel;
LOAD excel;` round-trip on first use. After that the binary is cached
in DuckDB's extension dir under `~/.duckdb/extensions/` and reused.

## One-statement template

```sql
-- optional: LOAD <ext> for non-default formats
COPY (SELECT * FROM read_parquet('data/in.parquet'))
  TO 'data/out.csv' (FORMAT csv, HEADER);
```

For format-to-format conversions where the source is also a file, the
inline `read_*` keeps everything in one statement:

```sql
COPY (FROM read_csv_auto('data/in.csv'))
  TO 'data/out.parquet';
```

## Partitioning

Add `PARTITION_BY (<col>, ...)` for hierarchical Parquet / CSV output:

```sql
COPY (FROM events) TO 'data/events_partitioned'
  (FORMAT parquet, PARTITION_BY (year, month), OVERWRITE_OR_IGNORE);
```

`OVERWRITE_OR_IGNORE` makes the call idempotent — a re-run with the
same data is a no-op.

## Compression

Parquet defaults to `snappy`. For smaller files at write-cost expense:

```sql
COPY (FROM data) TO 'data/out.parquet'
  (FORMAT parquet, CODEC 'zstd');
```

`zstd` typically lands 30–60 % smaller than `snappy` for analytical data.

## Remote sources

Reading from `s3://` / `https://` is one extra clause — `httpfs` is
pre-loaded:

```sql
COPY (
  SELECT * FROM read_parquet('s3://bucket/data/*.parquet')
) TO 'data/local.parquet';
```

For non-public buckets, see `s3.md` for the credential setup.

## Don't carry over from upstream

- The `find` shell wrapper for input file resolution — there is no Bash
  here; if the path the user gave is bare, ask them or use
  `bash.list_dir` from the `_system` skill.
- The `/duckdb-skills:install-duckdb` delegation — DuckDB is bundled
  with the vendored MCP; if `LOAD <ext>` fails, the operator extends
  `--init-sql` (skill content does NOT call `INSTALL` for spatial /
  httpfs, see `SKILL.md` Critical Rules).
