---
name: data_utils
description: >
  One-shot data diagnostics for a single table, no GraphQL by hand.
  Counts the number of rows / records in a table and answers similar
  single-number questions. Load this catalog when the user asks how
  many rows / records / items a table holds; joins, group-by, and
  dashboards belong in an analyst mission instead.
license: Apache-2.0
allowed-tools:
  - provider: task
    tools:
      - data_tables_rows_count
metadata:
  hugen:
    requires_skills: []
    recipe_catalog: true
    autoload: false
    tier_compatibility: [root, mission, worker]
compatibility:
  model: any
  runtime: hugen
---

# `data_utils` category

Loadable skill that admits Hugr Data Mesh utility recipes into the
session's tool catalog. Recipes themselves live as separate
task-eligible skills; this manifest is the discovery + permission
group that brings them into root.

Currently bundled:

- **`task:data_tables_rows_count(data_object)`** — counts rows in a
  named data object via the aggregation GraphQL field. Returns a
  single integer. Useful for daily diagnostics ("how many
  transactions yesterday?") or quick sanity checks before a deeper
  analysis.

## When to load

The user named a table and wants a count, schema-shape check, or
similarly tiny one-question answer about a single data object —
load this category, call the appropriate `task:*` tool, surface the
result. Bigger questions (joins, group-by, dashboards) still belong
in the analyst mission.

## How to call

Pass the user's request straight to the `task:*` tool. If the user
named a table, pass it as `name`; otherwise pass their natural-
language description as `query`. The recipe does its OWN table
lookup, schema check, and disambiguation — do NOT run discovery /
search tools or resolve the table yourself first, and do NOT load a
general data skill to "prepare" the call. That only duplicates the
recipe's internal work and wastes tool calls.
