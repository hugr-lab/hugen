---
name: duckdb-docs
license: MIT
description: >
  DuckDB SQL reference — function signatures, syntax (FILTER, QUALIFY,
  named parameters), list / struct / map / json types, date / time and
  string functions, window functions. No tools — pull this when you need
  to look up exact SQL syntax without running queries, or alongside
  `duckdb-data` while authoring queries you intend to execute.
allowed-tools: []
metadata:
  hugen:
    requires: []
    autoload: false
    autoload_for: []
    tier_compatibility: [mission, worker]
    sub_agents: []
    intents: [documentation]
compatibility:
  model: any
  runtime: hugen-phase-3.5
---

# DuckDB Docs

Pure reference skill — no tool grants. Use it via `skill_ref` to look up:

- function signatures and argument types,
- SQL clauses DuckDB extends beyond standard SQL (`FILTER`, `QUALIFY`,
  `USING SAMPLE`, `PIVOT` / `UNPIVOT`),
- composite types (`LIST`, `STRUCT`, `MAP`, `UNION`) and JSON functions,
- date / time / interval / string functions,
- window functions and `OVER` clause variants.

Load alongside `duckdb-data` when you are composing a real query, or
alone when the user asks a SQL-syntax question that does not need
execution.

> Content ported from upstream
> [`duckdb/duckdb-skills`](https://github.com/duckdb/duckdb-skills)
> (`skills/duckdb-docs/`, MIT).
