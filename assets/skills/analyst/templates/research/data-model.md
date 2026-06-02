# Data model

> Filled in by the researcher. Workers read this file FIRST and lift
> names verbatim — they must not re-discover what is recorded here.
> One subsection per table the mission touches. Use EXACT
> introspection names (never guessed/pluralised forms). External input
> files the mission analyses are recorded under their own section below.

## Sources & modules

<!-- Which Hugr data source / module backs each domain in the goal.
     One line each: <module> — what it tracks. -->

## Input data files

<!-- External data files the caller passed in (paths in [Inputs from
     caller]) for the mission to ANALYSE — a CSV / parquet / JSON the
     user already has, or data a prior step collected. One line each:
     <abs/path> — format + what it holds (+ key columns if known). The
     worker loads these via duckdb-data / python-runner; do NOT plan a
     wave to re-fetch them. "none" when the mission has no external
     input. -->

## Tables

<!-- One block per table the mission needs. Replace the placeholder
     block; add more as needed. Delete this comment when done. -->

### <type_name>

- **module**: <dotted.module.path>
- **query fields**: <select / aggregate / bucket_agg field names verbatim>
- **fields**:

  | name | type | semantics |
  |------|------|-----------|
  | <field> | <type> | <meaning; flag soft-delete / status / enum columns> |

- **join keys**: <field(s) here → target table.field>

## Join graph

<!-- How the tables above connect: table.field -> table.field.
     Confirmed join keys only. -->

<!-- query-shape note: this file records WHAT exists (exact names,
     types, join keys), not HOW to query it. Query grammar lives in
     the hugr-data references; runnable example queries go in
     queries.md. -->
